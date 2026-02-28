# Individualized Mailbox Intelligence System

**Document Version:** 1.0  
**Classification:** Technical Architecture  
**Created:** February 1, 2026  
**Component ID:** C016 - Mailbox-Level AI  

---

## Executive Summary

This document specifies an AI system that learns **individual mailbox behavior patterns** to maximize delivery and engagement for each subscriber. Rather than treating all Gmail users the same, the system understands that `john.smith@gmail.com` opens emails at 7am on his phone, prefers short subjects, and has a 3-day response window - while `jane.doe@gmail.com` opens at 9pm on desktop and responds within hours.

**Core Principle:** Every mailbox is unique. The AI must learn and adapt to each one individually.

---

## 1. Individualized Learning Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    INDIVIDUALIZED MAILBOX INTELLIGENCE                          │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│                         ┌─────────────────────┐                                 │
│                         │  SUBSCRIBER PROFILE │                                 │
│                         │    (Per Mailbox)    │                                 │
│                         └──────────┬──────────┘                                 │
│                                    │                                            │
│     ┌──────────────────────────────┼──────────────────────────────┐            │
│     │                              │                              │            │
│     ▼                              ▼                              ▼            │
│ ┌─────────────┐           ┌─────────────┐           ┌─────────────┐           │
│ │  BEHAVIORAL │           │  TEMPORAL   │           │  CONTENT    │           │
│ │   PATTERNS  │           │  PATTERNS   │           │ PREFERENCES │           │
│ │             │           │             │           │             │           │
│ │ • Open rate │           │ • Best hour │           │ • Subject   │           │
│ │ • Click rate│           │ • Best day  │           │   length    │           │
│ │ • Response  │           │ • Timezone  │           │ • Tone      │           │
│ │   time      │           │ • Frequency │           │ • Format    │           │
│ │ • Device    │           │   tolerance │           │ • Topics    │           │
│ └─────────────┘           └─────────────┘           └─────────────┘           │
│                                    │                                            │
│                                    ▼                                            │
│                    ┌───────────────────────────────┐                           │
│                    │   MAILBOX DELIVERY OPTIMIZER  │                           │
│                    │                               │                           │
│                    │  "For THIS subscriber, send   │                           │
│                    │   at THIS time, with THIS     │                           │
│                    │   content style, at THIS      │                           │
│                    │   frequency"                  │                           │
│                    └───────────────────────────────┘                           │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Individual Subscriber Profile Schema

### 2.1 Core Profile Structure

```go
// subscriber_profile.go

// SubscriberMailboxProfile contains all learned patterns for one mailbox
type SubscriberMailboxProfile struct {
    // Identity
    SubscriberID      string    `json:"subscriber_id"`
    Email             string    `json:"email"`
    EmailDomain       string    `json:"email_domain"`
    OrganizationID    string    `json:"organization_id"`
    ListID            string    `json:"list_id"`
    CreatedAt         time.Time `json:"created_at"`
    LastUpdated       time.Time `json:"last_updated"`
    
    // Engagement Profile (learned)
    Engagement        EngagementProfile        `json:"engagement"`
    
    // Temporal Profile (learned)
    Temporal          TemporalProfile          `json:"temporal"`
    
    // Content Preferences (learned)
    ContentPrefs      ContentPreferences       `json:"content_preferences"`
    
    // Delivery Profile (learned)
    Delivery          DeliveryProfile          `json:"delivery"`
    
    // Risk Assessment (learned)
    Risk              RiskProfile              `json:"risk"`
    
    // Predictive Scores (ML-generated)
    Predictions       PredictiveScores         `json:"predictions"`
    
    // Model Confidence
    ProfileMaturity   ProfileMaturity          `json:"maturity"`
}

// EngagementProfile tracks individual engagement patterns
type EngagementProfile struct {
    // Historical metrics
    TotalEmailsReceived    int       `json:"total_emails_received"`
    TotalOpens             int       `json:"total_opens"`
    TotalClicks            int       `json:"total_clicks"`
    TotalConversions       int       `json:"total_conversions"`
    
    // Calculated rates
    OpenRate               float64   `json:"open_rate"`
    ClickRate              float64   `json:"click_rate"`
    ClickToOpenRate        float64   `json:"click_to_open_rate"`
    
    // Engagement velocity (recent vs historical)
    RecentOpenRate30d      float64   `json:"recent_open_rate_30d"`
    RecentClickRate30d     float64   `json:"recent_click_rate_30d"`
    EngagementTrend        string    `json:"engagement_trend"`  // "increasing", "stable", "declining"
    
    // Response patterns
    AvgTimeToOpen          Duration  `json:"avg_time_to_open"`
    AvgTimeToClick         Duration  `json:"avg_time_to_click"`
    MedianTimeToOpen       Duration  `json:"median_time_to_open"`
    
    // Engagement score (0-100, ML-calculated)
    EngagementScore        float64   `json:"engagement_score"`
    ScoreLastCalculated    time.Time `json:"score_last_calculated"`
    
    // Reading behavior
    AvgReadTime            Duration  `json:"avg_read_time"`       // If trackable
    ScrollDepth            float64   `json:"scroll_depth"`        // If trackable
    
    // Last engagement
    LastOpenDate           *time.Time `json:"last_open_date"`
    LastClickDate          *time.Time `json:"last_click_date"`
    DaysSinceLastOpen      int        `json:"days_since_last_open"`
    DaysSinceLastClick     int        `json:"days_since_last_click"`
}

// TemporalProfile captures when this individual engages
type TemporalProfile struct {
    // Detected timezone
    InferredTimezone       string    `json:"inferred_timezone"`
    TimezoneConfidence     float64   `json:"timezone_confidence"`
    
    // Optimal send time (personalized)
    OptimalSendHourLocal   int       `json:"optimal_send_hour_local"`     // 0-23
    OptimalSendHourUTC     int       `json:"optimal_send_hour_utc"`
    OptimalSendDay         int       `json:"optimal_send_day"`            // 0=Sunday
    SendTimeConfidence     float64   `json:"send_time_confidence"`
    
    // Hour-by-hour engagement probability
    HourlyEngagementProb   [24]float64 `json:"hourly_engagement_prob"`    // P(open) per hour
    
    // Day-of-week patterns
    DayOfWeekEngagement    [7]float64  `json:"day_of_week_engagement"`    // P(open) per day
    
    // Weekend vs weekday
    WeekdayEngagement      float64   `json:"weekday_engagement"`
    WeekendEngagement      float64   `json:"weekend_engagement"`
    
    // Frequency tolerance
    OptimalFrequencyDays   int       `json:"optimal_frequency_days"`      // Days between emails
    FrequencyTolerance     string    `json:"frequency_tolerance"`         // "high", "medium", "low"
    LastEmailSent          time.Time `json:"last_email_sent"`
    EmailsThisWeek         int       `json:"emails_this_week"`
    EmailsThisMonth        int       `json:"emails_this_month"`
    
    // Response window
    TypicalResponseWindow  Duration  `json:"typical_response_window"`     // How long they take to engage
}

// ContentPreferences captures what content resonates with this individual
type ContentPreferences struct {
    // Subject line preferences
    PreferredSubjectLength    int       `json:"preferred_subject_length"`    // Characters
    SubjectLengthRange        [2]int    `json:"subject_length_range"`        // [min, max]
    
    // Subject elements that work
    RespondsToPersonalization bool      `json:"responds_to_personalization"` // {{first_name}} works?
    RespondsToEmoji           bool      `json:"responds_to_emoji"`
    RespondsToNumbers         bool      `json:"responds_to_numbers"`         // "5 tips", "30% off"
    RespondsToQuestions       bool      `json:"responds_to_questions"`       // "?" in subject
    RespondsToUrgency         bool      `json:"responds_to_urgency"`         // "Limited time"
    
    // Tone preferences (learned from engagement)
    PreferredTone             string    `json:"preferred_tone"`              // "formal", "casual", "friendly"
    
    // Content type preferences
    PreferredContentTypes     []string  `json:"preferred_content_types"`     // "promotional", "educational", "news"
    EngagementByContentType   map[string]float64 `json:"engagement_by_content_type"`
    
    // Topic interests (learned from clicks)
    TopicInterests            []TopicScore `json:"topic_interests"`
    
    // Email format
    PreferredFormat           string    `json:"preferred_format"`            // "html", "plain", "minimal"
    
    // CTA preferences
    PreferredCTAStyle         string    `json:"preferred_cta_style"`         // "button", "link", "multiple"
    ClickPositionPreference   string    `json:"click_position_preference"`   // "above_fold", "middle", "bottom"
}

// TopicScore represents interest level in a topic
type TopicScore struct {
    Topic      string  `json:"topic"`
    Score      float64 `json:"score"`       // 0-1 interest level
    ClickCount int     `json:"click_count"` // Clicks on this topic
    LastClick  time.Time `json:"last_click"`
}

// DeliveryProfile tracks delivery patterns for this mailbox
type DeliveryProfile struct {
    // Email provider details
    EmailProvider          string    `json:"email_provider"`     // "gmail", "outlook", "yahoo", "corporate"
    ProviderCategory       string    `json:"provider_category"`  // "consumer", "business", "edu"
    
    // Delivery history
    TotalDelivered         int       `json:"total_delivered"`
    TotalBounced           int       `json:"total_bounced"`
    TotalToSpam            int       `json:"total_to_spam"`      // If detectable
    DeliveryRate           float64   `json:"delivery_rate"`
    
    // Bounce details
    LastBounceDate         *time.Time `json:"last_bounce_date"`
    BounceType             string    `json:"bounce_type"`        // "hard", "soft", "none"
    ConsecutiveBounces     int       `json:"consecutive_bounces"`
    
    // Spam folder signals
    SpamRisk               float64   `json:"spam_risk"`          // 0-1 probability
    SpamSignals            []string  `json:"spam_signals"`       // Why we think spam risk
    
    // Inbox placement confidence
    InboxPlacementProb     float64   `json:"inbox_placement_prob"`
    
    // Device/client info
    PrimaryDevice          string    `json:"primary_device"`     // "mobile", "desktop", "tablet"
    PrimaryEmailClient     string    `json:"primary_email_client"` // "gmail_app", "apple_mail", "outlook"
    DeviceDistribution     map[string]float64 `json:"device_distribution"`
}

// RiskProfile assesses individual risk factors
type RiskProfile struct {
    // Churn risk
    ChurnRiskScore         float64   `json:"churn_risk_score"`   // 0-1 probability of unsubscribe
    ChurnRiskFactors       []string  `json:"churn_risk_factors"`
    
    // Complaint risk  
    ComplaintRiskScore     float64   `json:"complaint_risk_score"` // 0-1 probability of marking spam
    ComplaintHistory       bool      `json:"complaint_history"`
    
    // Bounce risk
    BounceRiskScore        float64   `json:"bounce_risk_score"`
    
    // Overall send risk (combined)
    OverallSendRisk        string    `json:"overall_send_risk"`  // "low", "medium", "high", "critical"
    
    // Recommended action
    RecommendedAction      string    `json:"recommended_action"` // "send", "reduce_frequency", "re-engage", "suppress"
    
    // List hygiene
    IsStale                bool      `json:"is_stale"`           // No engagement > 90 days
    IsHighlyEngaged        bool      `json:"is_highly_engaged"`  // Top 20% engagement
    RequiresReEngagement   bool      `json:"requires_re_engagement"`
}

// PredictiveScores contains ML-generated predictions
type PredictiveScores struct {
    // Will they open the next email?
    NextOpenProbability    float64   `json:"next_open_probability"`
    
    // Will they click?
    NextClickProbability   float64   `json:"next_click_probability"`
    
    // Will they convert?
    NextConversionProb     float64   `json:"next_conversion_probability"`
    
    // Lifetime value prediction
    PredictedLTV           float64   `json:"predicted_ltv"`
    
    // Days until likely churn
    DaysUntilChurn         *int      `json:"days_until_churn"`
    
    // Best next action
    RecommendedNextAction  string    `json:"recommended_next_action"`
    
    // Prediction timestamps
    PredictionsUpdated     time.Time `json:"predictions_updated"`
}

// ProfileMaturity indicates how much data we have
type ProfileMaturity struct {
    EmailsReceived         int       `json:"emails_received"`
    EngagementsRecorded    int       `json:"engagements_recorded"`
    DataPointsCollected    int       `json:"data_points_collected"`
    
    // Confidence levels
    EngagementConfidence   float64   `json:"engagement_confidence"`   // 0-1
    TemporalConfidence     float64   `json:"temporal_confidence"`
    ContentConfidence      float64   `json:"content_confidence"`
    
    // Maturity stage
    Stage                  string    `json:"stage"`  // "new", "learning", "established", "mature"
    
    // Minimum data thresholds met?
    HasMinimumData         bool      `json:"has_minimum_data"`
}
```

### 2.2 Database Schema

```sql
-- Individual mailbox intelligence table
CREATE TABLE mailing_subscriber_intelligence (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    
    -- Core profile (JSONB for flexibility)
    engagement_profile JSONB NOT NULL DEFAULT '{}',
    temporal_profile JSONB NOT NULL DEFAULT '{}',
    content_preferences JSONB NOT NULL DEFAULT '{}',
    delivery_profile JSONB NOT NULL DEFAULT '{}',
    risk_profile JSONB NOT NULL DEFAULT '{}',
    predictive_scores JSONB NOT NULL DEFAULT '{}',
    profile_maturity JSONB NOT NULL DEFAULT '{}',
    
    -- Quick access fields (denormalized for performance)
    engagement_score DECIMAL(5,2),
    optimal_send_hour_utc SMALLINT,
    churn_risk_score DECIMAL(5,4),
    next_open_probability DECIMAL(5,4),
    overall_send_risk VARCHAR(20),
    profile_stage VARCHAR(20) DEFAULT 'new',
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    last_engagement_at TIMESTAMP WITH TIME ZONE,
    last_prediction_at TIMESTAMP WITH TIME ZONE,
    
    -- Constraints
    UNIQUE(subscriber_id),
    
    -- Indexes for common queries
    INDEX idx_intelligence_org (organization_id),
    INDEX idx_intelligence_engagement (engagement_score DESC),
    INDEX idx_intelligence_churn (churn_risk_score DESC),
    INDEX idx_intelligence_send_hour (optimal_send_hour_utc),
    INDEX idx_intelligence_risk (overall_send_risk),
    INDEX idx_intelligence_stage (profile_stage)
);

-- Individual engagement events (for learning)
CREATE TABLE mailing_subscriber_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_id UUID NOT NULL,
    organization_id UUID NOT NULL,
    campaign_id UUID,
    email_id UUID NOT NULL,
    
    -- Event details
    event_type VARCHAR(20) NOT NULL,  -- 'delivered', 'open', 'click', 'unsubscribe', 'complaint', 'bounce'
    event_timestamp TIMESTAMP WITH TIME ZONE NOT NULL,
    
    -- Context at time of event
    send_hour_utc SMALLINT,
    send_day_of_week SMALLINT,
    device_type VARCHAR(20),
    email_client VARCHAR(50),
    
    -- Content context
    subject_line TEXT,
    subject_length INT,
    content_type VARCHAR(50),
    
    -- Click-specific
    link_url TEXT,
    link_position VARCHAR(20),
    
    -- Time metrics
    time_to_event_seconds INT,  -- Seconds from send to this event
    
    -- Metadata
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    -- Partitioning by month for performance
    INDEX idx_events_subscriber (subscriber_id, event_timestamp DESC),
    INDEX idx_events_type (event_type, event_timestamp DESC)
) PARTITION BY RANGE (event_timestamp);

-- Partition by month
CREATE TABLE mailing_subscriber_events_2026_01 PARTITION OF mailing_subscriber_events
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE mailing_subscriber_events_2026_02 PARTITION OF mailing_subscriber_events
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
-- ... additional partitions

-- Hourly engagement aggregates per subscriber (for pattern learning)
CREATE TABLE mailing_subscriber_hourly_patterns (
    subscriber_id UUID NOT NULL,
    hour_utc SMALLINT NOT NULL,  -- 0-23
    
    -- Engagement counts
    emails_sent INT DEFAULT 0,
    opens INT DEFAULT 0,
    clicks INT DEFAULT 0,
    
    -- Calculated rates
    open_rate DECIMAL(5,4),
    click_rate DECIMAL(5,4),
    
    -- Recency weighted score
    weighted_engagement_score DECIMAL(5,4),
    
    -- Last updated
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    PRIMARY KEY (subscriber_id, hour_utc)
);

-- Content preference learning
CREATE TABLE mailing_subscriber_content_responses (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_id UUID NOT NULL,
    
    -- Content characteristics
    subject_length_bucket VARCHAR(20),  -- 'short', 'medium', 'long'
    has_personalization BOOLEAN,
    has_emoji BOOLEAN,
    has_numbers BOOLEAN,
    has_question BOOLEAN,
    has_urgency BOOLEAN,
    content_type VARCHAR(50),
    topic_tags TEXT[],
    
    -- Response
    opened BOOLEAN NOT NULL,
    clicked BOOLEAN NOT NULL,
    
    -- Timestamp
    sent_at TIMESTAMP WITH TIME ZONE NOT NULL,
    
    INDEX idx_content_subscriber (subscriber_id, sent_at DESC)
);
```

---

## 3. Individual Learning Engine

### 3.1 Profile Builder

```go
// profile_builder.go

package intelligence

import (
    "context"
    "math"
    "time"
)

type IndividualProfileBuilder struct {
    db            *Database
    featureStore  *FeatureStore
    mlPredictor   *MLPredictor
}

// BuildProfile constructs/updates a complete profile for one subscriber
func (pb *IndividualProfileBuilder) BuildProfile(
    ctx context.Context,
    subscriberID string,
) (*SubscriberMailboxProfile, error) {
    
    // Fetch all events for this subscriber
    events, err := pb.db.GetSubscriberEvents(ctx, subscriberID, 365*24*time.Hour)
    if err != nil {
        return nil, fmt.Errorf("fetch events: %w", err)
    }
    
    // Initialize or load existing profile
    profile, err := pb.loadOrCreateProfile(ctx, subscriberID)
    if err != nil {
        return nil, fmt.Errorf("load profile: %w", err)
    }
    
    // Build each profile component
    profile.Engagement = pb.buildEngagementProfile(events)
    profile.Temporal = pb.buildTemporalProfile(events)
    profile.ContentPrefs = pb.buildContentPreferences(ctx, subscriberID, events)
    profile.Delivery = pb.buildDeliveryProfile(events)
    profile.Risk = pb.buildRiskProfile(profile)
    profile.Predictions = pb.generatePredictions(profile)
    profile.ProfileMaturity = pb.assessMaturity(profile, events)
    
    profile.LastUpdated = time.Now()
    
    // Persist
    if err := pb.saveProfile(ctx, profile); err != nil {
        return nil, fmt.Errorf("save profile: %w", err)
    }
    
    return profile, nil
}

// buildTemporalProfile learns when this individual engages
func (pb *IndividualProfileBuilder) buildTemporalProfile(
    events []SubscriberEvent,
) TemporalProfile {
    
    profile := TemporalProfile{
        HourlyEngagementProb: [24]float64{},
        DayOfWeekEngagement:  [7]float64{},
    }
    
    // Count engagements by hour and day
    hourlyOpens := make(map[int]int)
    hourlySends := make(map[int]int)
    dailyOpens := make(map[int]int)
    dailySends := make(map[int]int)
    
    var responseTimes []time.Duration
    var lastOpenTime *time.Time
    
    for _, event := range events {
        hour := event.EventTimestamp.Hour()
        day := int(event.EventTimestamp.Weekday())
        
        switch event.EventType {
        case "delivered":
            hourlySends[hour]++
            dailySends[day]++
            
        case "open":
            hourlyOpens[hour]++
            dailyOpens[day]++
            lastOpenTime = &event.EventTimestamp
            
            if event.TimeToEventSeconds > 0 {
                responseTimes = append(responseTimes, 
                    time.Duration(event.TimeToEventSeconds)*time.Second)
            }
        }
    }
    
    // Calculate hourly engagement probabilities
    var maxProb float64
    var bestHour int
    
    for hour := 0; hour < 24; hour++ {
        if hourlySends[hour] > 0 {
            prob := float64(hourlyOpens[hour]) / float64(hourlySends[hour])
            profile.HourlyEngagementProb[hour] = prob
            
            if prob > maxProb {
                maxProb = prob
                bestHour = hour
            }
        }
    }
    
    profile.OptimalSendHourUTC = bestHour
    
    // Calculate day-of-week engagement
    var weekdayTotal, weekdayOpens float64
    var weekendTotal, weekendOpens float64
    var bestDay int
    var bestDayProb float64
    
    for day := 0; day < 7; day++ {
        if dailySends[day] > 0 {
            prob := float64(dailyOpens[day]) / float64(dailySends[day])
            profile.DayOfWeekEngagement[day] = prob
            
            if prob > bestDayProb {
                bestDayProb = prob
                bestDay = day
            }
            
            if day >= 1 && day <= 5 { // Mon-Fri
                weekdayTotal += float64(dailySends[day])
                weekdayOpens += float64(dailyOpens[day])
            } else {
                weekendTotal += float64(dailySends[day])
                weekendOpens += float64(dailyOpens[day])
            }
        }
    }
    
    profile.OptimalSendDay = bestDay
    
    if weekdayTotal > 0 {
        profile.WeekdayEngagement = weekdayOpens / weekdayTotal
    }
    if weekendTotal > 0 {
        profile.WeekendEngagement = weekendOpens / weekendTotal
    }
    
    // Calculate response window
    if len(responseTimes) > 0 {
        sort.Slice(responseTimes, func(i, j int) bool {
            return responseTimes[i] < responseTimes[j]
        })
        
        // Median response time
        median := responseTimes[len(responseTimes)/2]
        profile.TypicalResponseWindow = median
    }
    
    // Infer timezone from peak activity
    profile.InferredTimezone = pb.inferTimezone(profile.HourlyEngagementProb)
    profile.TimezoneConfidence = pb.calculateTimezoneConfidence(profile.HourlyEngagementProb)
    
    // Calculate optimal frequency
    profile.OptimalFrequencyDays = pb.calculateOptimalFrequency(events)
    
    // Confidence based on data volume
    totalEvents := len(events)
    if totalEvents >= 50 {
        profile.SendTimeConfidence = 0.9
    } else if totalEvents >= 20 {
        profile.SendTimeConfidence = 0.7
    } else if totalEvents >= 10 {
        profile.SendTimeConfidence = 0.5
    } else {
        profile.SendTimeConfidence = 0.3
    }
    
    return profile
}

// buildContentPreferences learns what content resonates
func (pb *IndividualProfileBuilder) buildContentPreferences(
    ctx context.Context,
    subscriberID string,
    events []SubscriberEvent,
) ContentPreferences {
    
    prefs := ContentPreferences{
        EngagementByContentType: make(map[string]float64),
        TopicInterests:          []TopicScore{},
    }
    
    // Fetch content response data
    responses, _ := pb.db.GetContentResponses(ctx, subscriberID)
    
    // Analyze subject line length preference
    var shortOpens, shortSends int
    var mediumOpens, mediumSends int
    var longOpens, longSends int
    
    // Analyze personalization effectiveness
    var personalizationOpens, personalizationSends int
    var noPersonalizationOpens, noPersonalizationSends int
    
    // Analyze emoji effectiveness
    var emojiOpens, emojiSends int
    var noEmojiOpens, noEmojiSends int
    
    // Topic tracking
    topicClicks := make(map[string]int)
    
    for _, resp := range responses {
        // Subject length analysis
        switch resp.SubjectLengthBucket {
        case "short":
            shortSends++
            if resp.Opened { shortOpens++ }
        case "medium":
            mediumSends++
            if resp.Opened { mediumOpens++ }
        case "long":
            longSends++
            if resp.Opened { longOpens++ }
        }
        
        // Personalization analysis
        if resp.HasPersonalization {
            personalizationSends++
            if resp.Opened { personalizationOpens++ }
        } else {
            noPersonalizationSends++
            if resp.Opened { noPersonalizationOpens++ }
        }
        
        // Emoji analysis
        if resp.HasEmoji {
            emojiSends++
            if resp.Opened { emojiOpens++ }
        } else {
            noEmojiSends++
            if resp.Opened { noEmojiOpens++ }
        }
        
        // Topic interests
        if resp.Clicked {
            for _, topic := range resp.TopicTags {
                topicClicks[topic]++
            }
        }
    }
    
    // Determine preferred subject length
    shortRate := safeDiv(float64(shortOpens), float64(shortSends))
    mediumRate := safeDiv(float64(mediumOpens), float64(mediumSends))
    longRate := safeDiv(float64(longOpens), float64(longSends))
    
    if shortRate >= mediumRate && shortRate >= longRate {
        prefs.PreferredSubjectLength = 30
        prefs.SubjectLengthRange = [2]int{15, 40}
    } else if longRate >= mediumRate {
        prefs.PreferredSubjectLength = 70
        prefs.SubjectLengthRange = [2]int{50, 90}
    } else {
        prefs.PreferredSubjectLength = 50
        prefs.SubjectLengthRange = [2]int{35, 65}
    }
    
    // Personalization effectiveness
    persRate := safeDiv(float64(personalizationOpens), float64(personalizationSends))
    noPersRate := safeDiv(float64(noPersonalizationOpens), float64(noPersonalizationSends))
    prefs.RespondsToPersonalization = persRate > noPersRate * 1.1 // 10% lift threshold
    
    // Emoji effectiveness
    emojiRate := safeDiv(float64(emojiOpens), float64(emojiSends))
    noEmojiRate := safeDiv(float64(noEmojiOpens), float64(noEmojiSends))
    prefs.RespondsToEmoji = emojiRate > noEmojiRate * 1.1
    
    // Build topic interests
    for topic, clicks := range topicClicks {
        prefs.TopicInterests = append(prefs.TopicInterests, TopicScore{
            Topic:      topic,
            Score:      normalizeTopicScore(clicks),
            ClickCount: clicks,
        })
    }
    
    // Sort by score descending
    sort.Slice(prefs.TopicInterests, func(i, j int) bool {
        return prefs.TopicInterests[i].Score > prefs.TopicInterests[j].Score
    })
    
    // Keep top 10 topics
    if len(prefs.TopicInterests) > 10 {
        prefs.TopicInterests = prefs.TopicInterests[:10]
    }
    
    return prefs
}

// calculateOptimalFrequency determines ideal email frequency for this subscriber
func (pb *IndividualProfileBuilder) calculateOptimalFrequency(
    events []SubscriberEvent,
) int {
    
    // Group events by email/campaign
    emailDates := make(map[string]time.Time)
    emailOpened := make(map[string]bool)
    
    for _, event := range events {
        if event.EventType == "delivered" {
            emailDates[event.EmailID] = event.EventTimestamp
        }
        if event.EventType == "open" {
            emailOpened[event.EmailID] = true
        }
    }
    
    // Calculate engagement rate at different frequencies
    type frequencyBucket struct {
        sends int
        opens int
    }
    
    buckets := make(map[int]*frequencyBucket) // days since last email -> engagement
    
    var prevDate time.Time
    for emailID, date := range emailDates {
        if !prevDate.IsZero() {
            daysSince := int(date.Sub(prevDate).Hours() / 24)
            bucket := (daysSince / 3) * 3 // Bucket by 3-day intervals
            
            if buckets[bucket] == nil {
                buckets[bucket] = &frequencyBucket{}
            }
            
            buckets[bucket].sends++
            if emailOpened[emailID] {
                buckets[bucket].opens++
            }
        }
        prevDate = date
    }
    
    // Find frequency with best engagement
    var bestFreq int
    var bestRate float64
    
    for freq, bucket := range buckets {
        if bucket.sends >= 3 { // Minimum sample
            rate := float64(bucket.opens) / float64(bucket.sends)
            if rate > bestRate {
                bestRate = rate
                bestFreq = freq
            }
        }
    }
    
    if bestFreq == 0 {
        return 7 // Default to weekly
    }
    
    return bestFreq
}
```

### 3.2 Real-Time Profile Updater

```go
// profile_updater.go

package intelligence

// ProfileUpdater handles real-time profile updates on events
type ProfileUpdater struct {
    db           *Database
    cache        *redis.Client
    mlPredictor  *MLPredictor
}

// OnDeliveryEvent updates profile when email is delivered
func (pu *ProfileUpdater) OnDeliveryEvent(
    ctx context.Context,
    event DeliveryEvent,
) error {
    
    // Get current profile from cache or DB
    profile, err := pu.getProfile(ctx, event.SubscriberID)
    if err != nil {
        return err
    }
    
    // Update delivery stats
    profile.Engagement.TotalEmailsReceived++
    profile.Delivery.TotalDelivered++
    
    // Update frequency tracking
    profile.Temporal.LastEmailSent = event.Timestamp
    profile.Temporal.EmailsThisWeek++
    profile.Temporal.EmailsThisMonth++
    
    // Recalculate delivery rate
    profile.Delivery.DeliveryRate = float64(profile.Delivery.TotalDelivered) / 
        float64(profile.Engagement.TotalEmailsReceived)
    
    // Update maturity
    profile.ProfileMaturity.EmailsReceived++
    profile.ProfileMaturity.DataPointsCollected++
    
    // Save to cache for fast access
    pu.cacheProfile(ctx, profile)
    
    // Queue async DB update
    pu.queueProfileUpdate(ctx, profile)
    
    return nil
}

// OnOpenEvent updates profile when subscriber opens email
func (pu *ProfileUpdater) OnOpenEvent(
    ctx context.Context,
    event OpenEvent,
) error {
    
    profile, err := pu.getProfile(ctx, event.SubscriberID)
    if err != nil {
        return err
    }
    
    // Update engagement stats
    profile.Engagement.TotalOpens++
    profile.Engagement.LastOpenDate = &event.Timestamp
    profile.Engagement.DaysSinceLastOpen = 0
    
    // Update open rate (exponential moving average for recency weighting)
    newOpenRate := 1.0
    profile.Engagement.RecentOpenRate30d = ewma(
        profile.Engagement.RecentOpenRate30d, 
        newOpenRate, 
        0.1, // Smoothing factor
    )
    
    // Update response time
    if event.TimeToOpen > 0 {
        profile.Engagement.AvgTimeToOpen = ewmaDuration(
            profile.Engagement.AvgTimeToOpen,
            event.TimeToOpen,
            0.2,
        )
    }
    
    // Update hourly pattern
    hour := event.Timestamp.Hour()
    pu.updateHourlyPattern(ctx, event.SubscriberID, hour, "open")
    
    // Update device preference
    if event.DeviceType != "" {
        profile.Delivery.DeviceDistribution[event.DeviceType] = 
            profile.Delivery.DeviceDistribution[event.DeviceType] + 1
        profile.Delivery.PrimaryDevice = pu.getPrimaryDevice(profile.Delivery.DeviceDistribution)
    }
    
    // Recalculate engagement score
    profile.Engagement.EngagementScore = pu.calculateEngagementScore(profile)
    
    // Update engagement trend
    profile.Engagement.EngagementTrend = pu.calculateEngagementTrend(profile)
    
    // Update risk (opening reduces churn risk)
    profile.Risk.ChurnRiskScore = profile.Risk.ChurnRiskScore * 0.95 // Reduce by 5%
    
    // Update predictions
    profile.Predictions = pu.updatePredictions(profile)
    
    // Cache and persist
    pu.cacheProfile(ctx, profile)
    pu.queueProfileUpdate(ctx, profile)
    
    return nil
}

// OnClickEvent updates profile when subscriber clicks
func (pu *ProfileUpdater) OnClickEvent(
    ctx context.Context,
    event ClickEvent,
) error {
    
    profile, err := pu.getProfile(ctx, event.SubscriberID)
    if err != nil {
        return err
    }
    
    // Update click stats
    profile.Engagement.TotalClicks++
    profile.Engagement.LastClickDate = &event.Timestamp
    
    // Update click rate
    profile.Engagement.RecentClickRate30d = ewma(
        profile.Engagement.RecentClickRate30d,
        1.0,
        0.1,
    )
    
    // Update click-to-open rate
    if profile.Engagement.TotalOpens > 0 {
        profile.Engagement.ClickToOpenRate = float64(profile.Engagement.TotalClicks) / 
            float64(profile.Engagement.TotalOpens)
    }
    
    // Learn topic preference from clicked link
    if event.LinkTopic != "" {
        pu.updateTopicPreference(profile, event.LinkTopic)
    }
    
    // Learn click position preference
    if event.LinkPosition != "" {
        pu.updateClickPositionPreference(profile, event.LinkPosition)
    }
    
    // Engagement score boost
    profile.Engagement.EngagementScore = math.Min(100, 
        profile.Engagement.EngagementScore * 1.05) // 5% boost
    
    // Churn risk reduction (clicks indicate high interest)
    profile.Risk.ChurnRiskScore = profile.Risk.ChurnRiskScore * 0.90 // Reduce by 10%
    
    // Cache and persist
    pu.cacheProfile(ctx, profile)
    pu.queueProfileUpdate(ctx, profile)
    
    return nil
}

// OnUnsubscribeEvent handles unsubscribe (critical learning moment)
func (pu *ProfileUpdater) OnUnsubscribeEvent(
    ctx context.Context,
    event UnsubscribeEvent,
) error {
    
    profile, err := pu.getProfile(ctx, event.SubscriberID)
    if err != nil {
        return err
    }
    
    // Record the conditions that led to unsubscribe (for learning)
    churnContext := ChurnContext{
        SubscriberID:           event.SubscriberID,
        ChurnedAt:              event.Timestamp,
        EmailsReceivedLast30d:  profile.Temporal.EmailsThisMonth,
        DaysSinceLastOpen:      profile.Engagement.DaysSinceLastOpen,
        FrequencyAtChurn:       pu.calculateRecentFrequency(profile),
        EngagementScoreAtChurn: profile.Engagement.EngagementScore,
        LastEmailSubject:       event.TriggeringSubject,
        LastEmailContentType:   event.TriggeringContentType,
    }
    
    // Store for model training (learn what causes churn)
    pu.storeChurnContext(ctx, churnContext)
    
    // Update profile status
    profile.Risk.OverallSendRisk = "suppressed"
    profile.Risk.RecommendedAction = "do_not_send"
    
    pu.cacheProfile(ctx, profile)
    pu.queueProfileUpdate(ctx, profile)
    
    return nil
}

// calculateEngagementScore computes overall engagement (0-100)
func (pu *ProfileUpdater) calculateEngagementScore(profile *SubscriberMailboxProfile) float64 {
    
    var score float64
    
    // Recency component (40% weight)
    recencyScore := 100.0
    if profile.Engagement.DaysSinceLastOpen > 0 {
        // Half-life of 14 days
        recencyScore = 100.0 * math.Pow(0.5, float64(profile.Engagement.DaysSinceLastOpen)/14.0)
    }
    
    // Frequency component (30% weight)
    frequencyScore := math.Min(100, profile.Engagement.RecentOpenRate30d * 100 * 2)
    
    // Depth component (30% weight) - clicks indicate deeper engagement
    depthScore := math.Min(100, profile.Engagement.ClickToOpenRate * 100 * 3)
    
    score = (recencyScore * 0.4) + (frequencyScore * 0.3) + (depthScore * 0.3)
    
    return math.Max(0, math.Min(100, score))
}
```

---

## 4. Individual Send Optimization

### 4.1 Per-Subscriber Send Decision

```go
// individual_optimizer.go

package optimization

type IndividualSendOptimizer struct {
    profileStore    *ProfileStore
    mlPredictor     *MLPredictor
    contentAnalyzer *ContentAnalyzer
}

// OptimizeForSubscriber returns personalized send recommendations
func (iso *IndividualSendOptimizer) OptimizeForSubscriber(
    ctx context.Context,
    subscriberID string,
    campaign Campaign,
) (*IndividualSendPlan, error) {
    
    // Get subscriber's learned profile
    profile, err := iso.profileStore.GetProfile(ctx, subscriberID)
    if err != nil {
        return nil, fmt.Errorf("get profile: %w", err)
    }
    
    plan := &IndividualSendPlan{
        SubscriberID:    subscriberID,
        CampaignID:      campaign.ID,
        GeneratedAt:     time.Now(),
    }
    
    // 1. Should we send at all?
    plan.ShouldSend, plan.SkipReason = iso.decideShouldSend(profile, campaign)
    if !plan.ShouldSend {
        return plan, nil
    }
    
    // 2. When should we send? (personalized timing)
    plan.OptimalSendTime = iso.calculateOptimalSendTime(profile, campaign)
    
    // 3. What content optimizations? (personalized content)
    plan.ContentOptimizations = iso.optimizeContent(profile, campaign)
    
    // 4. Predicted performance
    plan.PredictedOpenRate = iso.predictOpenRate(profile, campaign, plan)
    plan.PredictedClickRate = iso.predictClickRate(profile, campaign, plan)
    
    // 5. Confidence level
    plan.Confidence = profile.ProfileMaturity.EngagementConfidence
    
    return plan, nil
}

// decideShouldSend determines if we should send to this subscriber
func (iso *IndividualSendOptimizer) decideShouldSend(
    profile *SubscriberMailboxProfile,
    campaign Campaign,
) (bool, string) {
    
    // Check suppression
    if profile.Risk.RecommendedAction == "do_not_send" {
        return false, "subscriber_suppressed"
    }
    
    // Check high bounce risk
    if profile.Risk.BounceRiskScore > 0.8 {
        return false, "high_bounce_risk"
    }
    
    // Check high complaint risk
    if profile.Risk.ComplaintRiskScore > 0.5 {
        return false, "high_complaint_risk"
    }
    
    // Check frequency - don't over-mail
    daysSinceLastEmail := time.Since(profile.Temporal.LastEmailSent).Hours() / 24
    if daysSinceLastEmail < float64(profile.Temporal.OptimalFrequencyDays) * 0.5 {
        // Sent less than half the optimal interval ago
        return false, "frequency_limit_reached"
    }
    
    // Check for stale subscribers (optional re-engagement logic)
    if profile.Risk.IsStale && !campaign.IsReEngagement {
        return false, "subscriber_stale_needs_reengagement"
    }
    
    // Check predicted engagement - skip likely non-engagers for campaign optimization
    if campaign.OptimizeDelivery && profile.Predictions.NextOpenProbability < 0.05 {
        return false, "predicted_non_engagement"
    }
    
    return true, ""
}

// calculateOptimalSendTime returns personalized send time
func (iso *IndividualSendOptimizer) calculateOptimalSendTime(
    profile *SubscriberMailboxProfile,
    campaign Campaign,
) time.Time {
    
    // If profile has high confidence on timing, use individual optimal hour
    if profile.ProfileMaturity.TemporalConfidence >= 0.7 {
        
        // Get subscriber's optimal hour in their timezone
        optimalHour := profile.Temporal.OptimalSendHourLocal
        optimalDay := profile.Temporal.OptimalSendDay
        
        // Find next occurrence of optimal day/hour
        sendTime := iso.findNextOccurrence(optimalDay, optimalHour, profile.Temporal.InferredTimezone)
        
        // Check if within campaign window
        if sendTime.Before(campaign.ScheduledStart) {
            sendTime = campaign.ScheduledStart
        }
        if sendTime.After(campaign.Deadline) {
            sendTime = campaign.Deadline
        }
        
        return sendTime
        
    } else {
        // Low confidence - use best hour from probability distribution
        bestHour := iso.selectFromDistribution(profile.Temporal.HourlyEngagementProb)
        return iso.findNextOccurrenceHour(bestHour, profile.Temporal.InferredTimezone)
    }
}

// optimizeContent returns content modifications for this subscriber
func (iso *IndividualSendOptimizer) optimizeContent(
    profile *SubscriberMailboxProfile,
    campaign Campaign,
) ContentOptimizations {
    
    opts := ContentOptimizations{}
    
    // Subject line optimizations
    opts.SubjectLine = iso.optimizeSubjectLine(profile, campaign.SubjectLine)
    
    // Personalization level
    opts.UsePersonalization = profile.ContentPrefs.RespondsToPersonalization
    
    // Preview text
    if profile.ContentPrefs.RespondsToPersonalization {
        opts.PreviewText = fmt.Sprintf("Hey %s, %s", 
            campaign.Subscriber.FirstName,
            campaign.PreviewText)
    } else {
        opts.PreviewText = campaign.PreviewText
    }
    
    // Content emphasis based on topic interests
    if len(profile.ContentPrefs.TopicInterests) > 0 {
        opts.EmphasizeTopics = iso.matchTopics(
            profile.ContentPrefs.TopicInterests,
            campaign.ContentTopics,
        )
    }
    
    return opts
}

// optimizeSubjectLine personalizes subject based on learned preferences
func (iso *IndividualSendOptimizer) optimizeSubjectLine(
    profile *SubscriberMailboxProfile,
    baseSubject string,
) string {
    
    subject := baseSubject
    
    // Adjust length if needed
    currentLength := len(subject)
    preferredLength := profile.ContentPrefs.PreferredSubjectLength
    
    if currentLength > profile.ContentPrefs.SubjectLengthRange[1] {
        // Subject too long for this subscriber - truncate intelligently
        subject = iso.truncateSubject(subject, preferredLength)
    }
    
    // Add emoji if subscriber responds to them
    if profile.ContentPrefs.RespondsToEmoji && !containsEmoji(subject) {
        subject = iso.addRelevantEmoji(subject)
    }
    
    // Remove emoji if subscriber doesn't respond
    if !profile.ContentPrefs.RespondsToEmoji && containsEmoji(subject) {
        subject = removeEmojis(subject)
    }
    
    // Add personalization if effective
    if profile.ContentPrefs.RespondsToPersonalization && !containsPersonalization(subject) {
        // Prepend name if it fits
        if len(subject) < preferredLength - 10 {
            subject = "{{first_name}}, " + subject
        }
    }
    
    return subject
}

// IndividualSendPlan contains personalized send instructions
type IndividualSendPlan struct {
    SubscriberID          string
    CampaignID            string
    GeneratedAt           time.Time
    
    // Send decision
    ShouldSend            bool
    SkipReason            string
    
    // Timing
    OptimalSendTime       time.Time
    SendTimeReason        string
    
    // Content
    ContentOptimizations  ContentOptimizations
    
    // Predictions
    PredictedOpenRate     float64
    PredictedClickRate    float64
    
    // Confidence
    Confidence            float64
}

type ContentOptimizations struct {
    SubjectLine        string
    PreviewText        string
    UsePersonalization bool
    EmphasizeTopics    []string
    ModifyLayout       bool
    LayoutPreference   string
}
```

### 4.2 Batch Campaign Optimization

```go
// campaign_optimizer.go

package optimization

// OptimizeCampaign creates individualized send plans for all subscribers
func (iso *IndividualSendOptimizer) OptimizeCampaign(
    ctx context.Context,
    campaign Campaign,
    subscribers []Subscriber,
) (*CampaignOptimizationResult, error) {
    
    result := &CampaignOptimizationResult{
        CampaignID:    campaign.ID,
        TotalSubscribers: len(subscribers),
        SendPlans:     make([]*IndividualSendPlan, 0, len(subscribers)),
        Schedule:      make(map[time.Time][]string), // time -> subscriber IDs
    }
    
    // Process in parallel
    planCh := make(chan *IndividualSendPlan, len(subscribers))
    errCh := make(chan error, 1)
    
    // Worker pool
    workerCount := 50
    subscriberCh := make(chan Subscriber, len(subscribers))
    
    for i := 0; i < workerCount; i++ {
        go func() {
            for sub := range subscriberCh {
                plan, err := iso.OptimizeForSubscriber(ctx, sub.ID, campaign)
                if err != nil {
                    select {
                    case errCh <- err:
                    default:
                    }
                    continue
                }
                planCh <- plan
            }
        }()
    }
    
    // Feed subscribers
    for _, sub := range subscribers {
        subscriberCh <- sub
    }
    close(subscriberCh)
    
    // Collect results
    for i := 0; i < len(subscribers); i++ {
        select {
        case plan := <-planCh:
            result.SendPlans = append(result.SendPlans, plan)
            
            if plan.ShouldSend {
                result.ToSend++
                // Group by send time (rounded to 15-min intervals)
                roundedTime := plan.OptimalSendTime.Truncate(15 * time.Minute)
                result.Schedule[roundedTime] = append(result.Schedule[roundedTime], plan.SubscriberID)
            } else {
                result.Skipped++
                result.SkipReasons[plan.SkipReason]++
            }
            
        case err := <-errCh:
            return nil, err
        }
    }
    
    // Calculate aggregate predictions
    result.PredictedOverallOpenRate = iso.calculatePredictedOpenRate(result.SendPlans)
    result.PredictedOverallClickRate = iso.calculatePredictedClickRate(result.SendPlans)
    
    // Optimize schedule distribution (avoid spikes)
    result.Schedule = iso.balanceSchedule(result.Schedule, campaign)
    
    return result, nil
}

// CampaignOptimizationResult contains the full campaign optimization
type CampaignOptimizationResult struct {
    CampaignID               string
    TotalSubscribers         int
    ToSend                   int
    Skipped                  int
    SkipReasons              map[string]int
    
    SendPlans                []*IndividualSendPlan
    Schedule                 map[time.Time][]string
    
    PredictedOverallOpenRate float64
    PredictedOverallClickRate float64
    
    // Optimization details
    UniqueOptimalHours       int   // How many different hours are used
    PersonalizationApplied   int   // How many got personalized content
    TimingPersonalized       int   // How many got personalized timing
}
```

---

## 5. ML Models for Individual Prediction

### 5.1 Per-Subscriber Open Prediction Model

```yaml
model_id: MDL-IND-001
name: "Individual Open Predictor"
granularity: "Per subscriber per email"
type: "Personalized Classification"

objective:
  predict: "Will this specific subscriber open this specific email?"
  output: "Probability [0, 1]"

features:
  subscriber_profile_features:
    # From learned profile
    - engagement_score                    # Overall engagement health
    - days_since_last_open                # Recency
    - recent_open_rate_30d                # Recent behavior
    - historical_open_rate                # Long-term behavior
    - typical_response_window_hours       # How long they take
    
    # Temporal match
    - hour_match_score                    # Is send hour optimal for them?
    - day_match_score                     # Is send day optimal?
    - within_response_window              # Are we in their active window?
    
    # Frequency factors
    - days_since_last_email               # Time since we emailed
    - optimal_frequency_match             # Are we at their ideal frequency?
    - emails_received_this_week           # Saturation risk
    
    # Content match
    - subject_length_match                # Does subject match preference?
    - personalization_present             # Do they respond to it?
    - emoji_match                         # Emoji preference match
    - topic_relevance_score               # How relevant is content?
    
    # Profile maturity
    - profile_confidence                  # How much do we know?
    
  email_features:
    - subject_length
    - has_personalization
    - has_emoji
    - content_type
    - campaign_type
    
  contextual_features:
    - send_hour_local
    - send_day_of_week
    - is_weekend
    - is_holiday

model_architecture:
  type: "Gradient Boosted Trees + Embedding"
  
  subscriber_embedding:
    purpose: "Learn latent subscriber representation"
    dimensions: 32
    trained_on: "Engagement history"
    
  prediction_model:
    algorithm: "LightGBM"
    features: "Profile features + subscriber embedding"
    
training:
  approach: "Continuous online learning"
  update_frequency: "After each engagement event"
  
  cold_start_handling:
    new_subscribers: "Use segment-level average"
    fallback_features:
      - email_domain_average_open_rate
      - list_average_open_rate
      - similar_subscriber_rate
      
inference:
  latency: "<5ms per subscriber"
  batch_support: "Up to 10,000 subscribers"
  caching: "Profile features cached in Redis"
```

### 5.2 Individual Churn Prediction

```yaml
model_id: MDL-IND-002
name: "Individual Churn Predictor"
granularity: "Per subscriber"
type: "Survival Analysis / Time-to-Event"

objective:
  predict: 
    - "Will this subscriber unsubscribe/complain?"
    - "When are they likely to churn?"
  output:
    - churn_probability: "P(churn in next 30 days)"
    - expected_days_to_churn: "If they will churn, when?"

features:
  engagement_trajectory:
    - engagement_score_current
    - engagement_score_30d_ago
    - engagement_score_60d_ago
    - engagement_trend                    # "declining", "stable", "improving"
    - engagement_velocity                 # Rate of change
    
  warning_signals:
    - consecutive_unopened_emails
    - days_since_last_engagement
    - decreasing_time_to_open             # Taking longer to open
    - decreasing_click_rate
    - decreased_scroll_depth
    
  frequency_factors:
    - emails_received_vs_optimal          # Over or under optimal?
    - recent_frequency_increase           # Did we increase frequency?
    - fatigue_score                       # Computed fatigue indicator
    
  content_factors:
    - recent_content_relevance_scores
    - topic_mismatch_rate
    
  historical_patterns:
    - previous_unsubscribe_attempts
    - previous_complaints
    - subscriber_tenure_days
    
output:
  churn_risk_score: 0.35
  risk_level: "medium"
  days_until_likely_churn: 45
  primary_risk_factors:
    - "5 consecutive unopened emails"
    - "Engagement declining 15% per week"
  recommended_actions:
    - "Reduce frequency from 3x to 1x per week"
    - "Send re-engagement campaign"
    - "Change content type"
```

---

## 6. Feedback and Continuous Learning

### 6.1 Learning from Every Interaction

```go
// individual_learner.go

package learning

type IndividualLearner struct {
    profileStore *ProfileStore
    modelTrainer *ModelTrainer
    patternStore *PatternStore
}

// LearnFromOpen updates individual model when subscriber opens
func (il *IndividualLearner) LearnFromOpen(
    ctx context.Context,
    event OpenEvent,
) error {
    
    // Get send context (what we predicted/decided)
    sendContext, err := il.getSendContext(ctx, event.EmailID)
    if err != nil {
        return err
    }
    
    // 1. Learn timing pattern
    il.learnTimingPattern(ctx, event.SubscriberID, TimingOutcome{
        SentHour:       sendContext.SentHour,
        OpenedHour:     event.Timestamp.Hour(),
        TimeToOpen:     event.TimeToOpen,
        WasPredictedOptimal: sendContext.WasOptimalTime,
        Outcome:        "opened",
    })
    
    // 2. Learn content preference
    if sendContext.SubjectLine != "" {
        il.learnContentPreference(ctx, event.SubscriberID, ContentOutcome{
            SubjectLength:    len(sendContext.SubjectLine),
            HadPersonalization: sendContext.HadPersonalization,
            HadEmoji:        containsEmoji(sendContext.SubjectLine),
            ContentType:     sendContext.ContentType,
            Topics:          sendContext.Topics,
            Outcome:         "opened",
        })
    }
    
    // 3. Update prediction accuracy tracking
    il.trackPredictionAccuracy(ctx, PredictionOutcome{
        SubscriberID:     event.SubscriberID,
        PredictedProb:    sendContext.PredictedOpenRate,
        ActualOutcome:    1.0, // Opened
        Model:            "individual_open_predictor",
    })
    
    // 4. Store for batch model retraining
    il.storeTrainingExample(ctx, TrainingExample{
        SubscriberID: event.SubscriberID,
        Features:     sendContext.Features,
        Label:        1, // Opened
        Timestamp:    event.Timestamp,
    })
    
    return nil
}

// LearnFromNonOpen updates model when subscriber doesn't open (after window expires)
func (il *IndividualLearner) LearnFromNonOpen(
    ctx context.Context,
    emailID string,
    subscriberID string,
) error {
    
    sendContext, err := il.getSendContext(ctx, emailID)
    if err != nil {
        return err
    }
    
    // Only count as non-open if outside their typical response window
    profile, _ := il.profileStore.GetProfile(ctx, subscriberID)
    if profile != nil {
        responseWindow := profile.Temporal.TypicalResponseWindow
        if time.Since(sendContext.SentAt) < responseWindow * 2 {
            // Still might open, don't count yet
            return nil
        }
    }
    
    // Learn from non-open
    il.learnTimingPattern(ctx, subscriberID, TimingOutcome{
        SentHour:            sendContext.SentHour,
        WasPredictedOptimal: sendContext.WasOptimalTime,
        Outcome:             "not_opened",
    })
    
    il.learnContentPreference(ctx, subscriberID, ContentOutcome{
        SubjectLength:       len(sendContext.SubjectLine),
        HadPersonalization:  sendContext.HadPersonalization,
        ContentType:         sendContext.ContentType,
        Outcome:             "not_opened",
    })
    
    il.trackPredictionAccuracy(ctx, PredictionOutcome{
        SubscriberID:  subscriberID,
        PredictedProb: sendContext.PredictedOpenRate,
        ActualOutcome: 0.0, // Not opened
        Model:         "individual_open_predictor",
    })
    
    il.storeTrainingExample(ctx, TrainingExample{
        SubscriberID: subscriberID,
        Features:     sendContext.Features,
        Label:        0, // Not opened
        Timestamp:    time.Now(),
    })
    
    return nil
}

// learnTimingPattern updates individual timing model
func (il *IndividualLearner) learnTimingPattern(
    ctx context.Context,
    subscriberID string,
    outcome TimingOutcome,
) {
    
    // Update hourly pattern in database
    if outcome.Outcome == "opened" {
        // Positive signal for this hour
        il.patternStore.IncrementHourlyEngagement(ctx, subscriberID, outcome.OpenedHour)
    }
    
    // Track if our optimal hour prediction was correct
    if outcome.WasPredictedOptimal {
        if outcome.Outcome == "opened" {
            // Our prediction was good
            il.patternStore.RecordTimingPredictionSuccess(ctx, subscriberID)
        } else {
            // Our prediction was wrong - need to learn
            il.patternStore.RecordTimingPredictionFailure(ctx, subscriberID, outcome.SentHour)
        }
    }
}
```

### 6.2 Pattern Consolidation (Batch Learning)

```go
// pattern_consolidator.go

package learning

// ConsolidatePatterns runs periodically to update individual models
func (pc *PatternConsolidator) ConsolidatePatterns(ctx context.Context) error {
    
    // Get all subscribers with recent activity
    subscribers, err := pc.db.GetActiveSubscribers(ctx, 30*24*time.Hour)
    if err != nil {
        return err
    }
    
    log.Info().Int("count", len(subscribers)).Msg("Consolidating patterns")
    
    for _, subscriberID := range subscribers {
        if err := pc.consolidateSubscriberPatterns(ctx, subscriberID); err != nil {
            log.Warn().Err(err).Str("subscriber", subscriberID).Msg("Failed to consolidate")
            continue
        }
    }
    
    return nil
}

func (pc *PatternConsolidator) consolidateSubscriberPatterns(
    ctx context.Context,
    subscriberID string,
) error {
    
    // Get all recent events for this subscriber
    events, err := pc.db.GetSubscriberEvents(ctx, subscriberID, 90*24*time.Hour)
    if err != nil {
        return err
    }
    
    if len(events) < 5 {
        // Not enough data yet
        return nil
    }
    
    // Rebuild hourly engagement distribution
    hourlyEngagement := make(map[int]*hourlyStats)
    for hour := 0; hour < 24; hour++ {
        hourlyEngagement[hour] = &hourlyStats{}
    }
    
    for _, event := range events {
        hour := event.EventTimestamp.Hour()
        
        if event.EventType == "delivered" {
            hourlyEngagement[hour].sent++
        }
        if event.EventType == "open" {
            hourlyEngagement[hour].opened++
        }
    }
    
    // Calculate hourly probabilities with Bayesian smoothing
    var probs [24]float64
    prior := 0.2 // Prior open rate assumption
    priorStrength := 5.0 // Equivalent to 5 observations
    
    for hour := 0; hour < 24; hour++ {
        stats := hourlyEngagement[hour]
        if stats.sent > 0 {
            // Bayesian smoothed probability
            probs[hour] = (float64(stats.opened) + prior*priorStrength) / 
                (float64(stats.sent) + priorStrength)
        } else {
            probs[hour] = prior
        }
    }
    
    // Find optimal hour
    var bestHour int
    var bestProb float64
    for hour, prob := range probs {
        if prob > bestProb {
            bestProb = prob
            bestHour = hour
        }
    }
    
    // Update profile
    profile, err := pc.profileStore.GetProfile(ctx, subscriberID)
    if err != nil {
        return err
    }
    
    profile.Temporal.HourlyEngagementProb = probs
    profile.Temporal.OptimalSendHourUTC = bestHour
    
    // Calculate confidence based on data volume
    totalEvents := len(events)
    if totalEvents >= 50 {
        profile.Temporal.SendTimeConfidence = 0.9
    } else if totalEvents >= 20 {
        profile.Temporal.SendTimeConfidence = 0.7
    } else {
        profile.Temporal.SendTimeConfidence = 0.5
    }
    
    return pc.profileStore.SaveProfile(ctx, profile)
}
```

---

## 7. API Endpoints

### 7.1 Individual Intelligence API

```yaml
openapi: 3.0.0
info:
  title: Individual Mailbox Intelligence API
  version: 1.0.0

paths:
  /api/v1/subscribers/{subscriberId}/intelligence:
    get:
      summary: Get subscriber's learned profile
      parameters:
        - name: subscriberId
          in: path
          required: true
          schema:
            type: string
      responses:
        '200':
          description: Subscriber intelligence profile
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/SubscriberIntelligence'

  /api/v1/subscribers/{subscriberId}/optimal-send-time:
    get:
      summary: Get optimal send time for subscriber
      parameters:
        - name: subscriberId
          in: path
          required: true
        - name: timezone
          in: query
          description: Return time in this timezone
          schema:
            type: string
            default: UTC
      responses:
        '200':
          description: Optimal send time
          content:
            application/json:
              schema:
                type: object
                properties:
                  optimal_hour_utc:
                    type: integer
                  optimal_hour_local:
                    type: integer
                  optimal_day:
                    type: string
                  confidence:
                    type: number
                  hourly_probabilities:
                    type: array
                    items:
                      type: number

  /api/v1/subscribers/{subscriberId}/predict-engagement:
    post:
      summary: Predict engagement for specific email
      parameters:
        - name: subscriberId
          in: path
          required: true
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                subject_line:
                  type: string
                content_type:
                  type: string
                send_time:
                  type: string
                  format: date-time
      responses:
        '200':
          description: Engagement prediction
          content:
            application/json:
              schema:
                type: object
                properties:
                  open_probability:
                    type: number
                  click_probability:
                    type: number
                  recommendations:
                    type: array
                    items:
                      type: string

  /api/v1/campaigns/{campaignId}/optimize:
    post:
      summary: Generate individualized send plans for campaign
      parameters:
        - name: campaignId
          in: path
          required: true
      responses:
        '200':
          description: Campaign optimization result
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/CampaignOptimization'

components:
  schemas:
    SubscriberIntelligence:
      type: object
      properties:
        subscriber_id:
          type: string
        engagement:
          type: object
          properties:
            score:
              type: number
            open_rate:
              type: number
            click_rate:
              type: number
            trend:
              type: string
        temporal:
          type: object
          properties:
            optimal_send_hour:
              type: integer
            timezone:
              type: string
            best_days:
              type: array
              items:
                type: string
        content_preferences:
          type: object
          properties:
            preferred_subject_length:
              type: integer
            responds_to_personalization:
              type: boolean
            responds_to_emoji:
              type: boolean
            topic_interests:
              type: array
              items:
                type: string
        predictions:
          type: object
          properties:
            next_open_probability:
              type: number
            churn_risk:
              type: number
        confidence:
          type: number
```

---

## 8. Dashboard Visualization

### 8.1 Individual Subscriber Intelligence View

```typescript
// SubscriberIntelligenceView.tsx

interface SubscriberIntelligenceProps {
  subscriberId: string;
}

export const SubscriberIntelligenceView: React.FC<SubscriberIntelligenceProps> = ({
  subscriberId
}) => {
  const { data: intelligence } = useSubscriberIntelligence(subscriberId);
  
  if (!intelligence) return <Loading />;
  
  return (
    <div className="subscriber-intelligence">
      <header>
        <h2>{intelligence.email}</h2>
        <ProfileMaturityBadge stage={intelligence.maturity.stage} />
      </header>
      
      {/* Engagement Health */}
      <Card title="Engagement Health">
        <EngagementScoreGauge score={intelligence.engagement.score} />
        <div className="metrics-row">
          <Metric 
            label="Open Rate" 
            value={`${(intelligence.engagement.open_rate * 100).toFixed(1)}%`}
            trend={intelligence.engagement.trend}
          />
          <Metric 
            label="Click Rate" 
            value={`${(intelligence.engagement.click_rate * 100).toFixed(1)}%`}
          />
          <Metric 
            label="Days Since Open" 
            value={intelligence.engagement.days_since_last_open}
            warning={intelligence.engagement.days_since_last_open > 30}
          />
        </div>
      </Card>
      
      {/* Optimal Send Time */}
      <Card title="Best Time to Send">
        <HourlyEngagementChart 
          data={intelligence.temporal.hourly_engagement_prob}
          optimalHour={intelligence.temporal.optimal_send_hour}
        />
        <p className="insight">
          This subscriber is most likely to engage at{' '}
          <strong>{formatHour(intelligence.temporal.optimal_send_hour)}</strong>
          {' '}({intelligence.temporal.timezone})
        </p>
        <DayOfWeekChart data={intelligence.temporal.day_of_week_engagement} />
      </Card>
      
      {/* Content Preferences */}
      <Card title="Content Preferences">
        <PreferenceList>
          <PreferenceItem 
            label="Subject Length"
            value={`${intelligence.content.preferred_subject_length} chars`}
            icon="📏"
          />
          <PreferenceItem 
            label="Personalization"
            value={intelligence.content.responds_to_personalization ? 'Effective' : 'Not effective'}
            positive={intelligence.content.responds_to_personalization}
          />
          <PreferenceItem 
            label="Emoji"
            value={intelligence.content.responds_to_emoji ? 'Responds well' : 'No impact'}
            positive={intelligence.content.responds_to_emoji}
          />
        </PreferenceList>
        
        {intelligence.content.topic_interests.length > 0 && (
          <TopicInterests topics={intelligence.content.topic_interests} />
        )}
      </Card>
      
      {/* Predictions */}
      <Card title="AI Predictions">
        <PredictionRow>
          <Prediction 
            label="Will open next email"
            probability={intelligence.predictions.next_open_probability}
          />
          <Prediction 
            label="Churn risk"
            probability={intelligence.risk.churn_risk_score}
            inverse
          />
        </PredictionRow>
        
        {intelligence.risk.churn_risk_score > 0.5 && (
          <Alert type="warning">
            <strong>High churn risk detected.</strong>
            <p>Recommended: {intelligence.risk.recommended_action}</p>
          </Alert>
        )}
      </Card>
      
      {/* Profile Confidence */}
      <Card title="AI Confidence">
        <ConfidenceMeters>
          <ConfidenceMeter 
            label="Engagement patterns"
            value={intelligence.maturity.engagement_confidence}
          />
          <ConfidenceMeter 
            label="Timing patterns"
            value={intelligence.maturity.temporal_confidence}
          />
          <ConfidenceMeter 
            label="Content preferences"
            value={intelligence.maturity.content_confidence}
          />
        </ConfidenceMeters>
        <p className="data-note">
          Based on {intelligence.maturity.emails_received} emails and{' '}
          {intelligence.maturity.engagements_recorded} engagements
        </p>
      </Card>
    </div>
  );
};
```

---

## 9. Success Metrics

### 9.1 Individual Intelligence KPIs

| Metric | Target | How Measured |
|--------|--------|--------------|
| Individual timing accuracy | >80% | % of sends at predicted optimal time that engage |
| Personalization lift | +25% | A/B test: personalized vs generic |
| Churn prediction accuracy | >75% AUC | ROC on actual churns |
| Frequency optimization | -20% unsubscribes | Compare optimized vs fixed frequency |
| Per-subscriber open rate lift | +30% | vs. campaign-level averages |
| Profile maturity (>10 interactions) | 60% of actives | % of subscribers with mature profiles |

---

## 10. Implementation Priority

| Phase | Focus | Duration |
|-------|-------|----------|
| **1** | Profile schema, basic learning, temporal patterns | 2 weeks |
| **2** | Content preference learning, engagement scoring | 2 weeks |
| **3** | Individual ML predictions, send optimization | 3 weeks |
| **4** | Full campaign optimization, dashboard | 2 weeks |
| **5** | Advanced churn prediction, continuous learning | 2 weeks |

---

**Document End**
