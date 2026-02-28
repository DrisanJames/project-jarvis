# Intelligent Sending Plan System

**Document Version:** 1.0  
**Classification:** Technical Architecture  
**Created:** February 1, 2026  
**Component ID:** C018 - Sending Plan Generator  

---

## Executive Summary

This document specifies an **Intelligent Sending Plan** system that analyzes all available metrics, audience data, offer performance, and sending constraints to generate comprehensive, approvable sending plans. Rather than per-inbox decisions, the system presents **high-level strategic options** organized by time periods (morning, first half, full day) for human review and approval.

**Core Concept:** AI synthesizes everything it knows to propose "Here's what I recommend sending today, and why."

---

## 1. Sending Plan Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      INTELLIGENT SENDING PLAN GENERATOR                         │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                         DATA SYNTHESIS LAYER                             │   │
│  │                                                                          │   │
│  │  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐      │   │
│  │  │  Volume  │ │ Audience │ │  Offer   │ │ Timing   │ │ Content  │      │   │
│  │  │ Capacity │ │ Segments │ │  Perf.   │ │ Patterns │ │  Perf.   │      │   │
│  │  └──────────┘ └──────────┘ └──────────┘ └──────────┘ └──────────┘      │   │
│  │        │           │            │            │            │              │   │
│  │        └───────────┴────────────┴────────────┴────────────┘              │   │
│  │                                 │                                         │   │
│  │                                 ▼                                         │   │
│  │                    ┌─────────────────────────┐                           │   │
│  │                    │    PLAN OPTIMIZER       │                           │   │
│  │                    │                         │                           │   │
│  │                    │  Maximize: Revenue      │                           │   │
│  │                    │  Respect: Constraints   │                           │   │
│  │                    │  Balance: Risk/Reward   │                           │   │
│  │                    └─────────────────────────┘                           │   │
│  │                                 │                                         │   │
│  └─────────────────────────────────┼─────────────────────────────────────────┘   │
│                                    │                                            │
│                                    ▼                                            │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                       SENDING PLAN OUTPUT                                │   │
│  │                                                                          │   │
│  │   ┌─────────────┐    ┌─────────────┐    ┌─────────────┐                 │   │
│  │   │   MORNING   │    │  FIRST HALF │    │  FULL DAY   │                 │   │
│  │   │    PLAN     │    │    PLAN     │    │    PLAN     │                 │   │
│  │   │             │    │             │    │             │                 │   │
│  │   │  6AM-12PM   │    │  6AM-3PM    │    │  6AM-10PM   │                 │   │
│  │   │             │    │             │    │             │                 │   │
│  │   │ [APPROVE]   │    │ [APPROVE]   │    │ [APPROVE]   │                 │   │
│  │   └─────────────┘    └─────────────┘    └─────────────┘                 │   │
│  │                                                                          │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Data Inputs for Plan Generation

### 2.1 Volume Capacity Analysis

```go
// volume_analyzer.go

package plan

type VolumeAnalyzer struct {
    db             *Database
    deliveryServers *DeliveryServerStore
}

// VolumeCapacity represents available sending capacity
type VolumeCapacity struct {
    // Overall limits
    DailyLimit              int       // Max emails allowed today
    RemainingToday          int       // How many more we can send
    UsedToday               int       // Already sent today
    
    // ESP-specific limits
    ESPCapacity             map[string]*ESPCapacity
    
    // Domain/IP warmup constraints
    WarmupConstraints       []WarmupConstraint
    
    // Time-based distribution
    HourlyRecommendation    [24]int   // Recommended volume per hour
    
    // List-specific limits
    ListLimits              map[string]int  // Per-list daily limits
}

type ESPCapacity struct {
    ESP                     string    // "sparkpost", "ses"
    DailyLimit              int
    HourlyLimit             int
    RemainingDaily          int
    RemainingHourly         int
    ReputationScore         float64
    Status                  string    // "healthy", "warming", "throttled"
}

type WarmupConstraint struct {
    Domain                  string
    IPAddress               string
    CurrentStage            string    // "cold", "warming", "established"
    DailyLimit              int
    RecommendedVolume       int
    DaysInStage             int
}

// AnalyzeCapacity calculates available sending capacity
func (va *VolumeAnalyzer) AnalyzeCapacity(ctx context.Context) (*VolumeCapacity, error) {
    
    capacity := &VolumeCapacity{
        ESPCapacity:         make(map[string]*ESPCapacity),
        ListLimits:          make(map[string]int),
        HourlyRecommendation: [24]int{},
    }
    
    // Get overall account limits
    accountLimits := va.db.GetAccountLimits(ctx)
    capacity.DailyLimit = accountLimits.MaxDailyEmails
    capacity.UsedToday = va.db.GetTodaySendCount(ctx)
    capacity.RemainingToday = capacity.DailyLimit - capacity.UsedToday
    
    // Analyze each ESP
    servers, _ := va.deliveryServers.GetActiveServers(ctx)
    for _, server := range servers {
        espCap := &ESPCapacity{
            ESP:             server.Type,
            DailyLimit:      server.DailyQuota,
            HourlyLimit:     server.HourlyQuota,
            RemainingDaily:  server.DailyQuota - server.UsedToday,
            RemainingHourly: server.HourlyQuota - server.UsedThisHour,
            ReputationScore: server.ReputationScore,
            Status:          server.Status,
        }
        capacity.ESPCapacity[server.ID] = espCap
    }
    
    // Get warmup constraints
    warmupDomains, _ := va.db.GetWarmupDomains(ctx)
    for _, domain := range warmupDomains {
        capacity.WarmupConstraints = append(capacity.WarmupConstraints, WarmupConstraint{
            Domain:            domain.Name,
            CurrentStage:      domain.WarmupStage,
            DailyLimit:        domain.CurrentDailyLimit,
            RecommendedVolume: domain.RecommendedVolume,
            DaysInStage:       domain.DaysInCurrentStage,
        })
    }
    
    // Calculate optimal hourly distribution
    capacity.HourlyRecommendation = va.calculateHourlyDistribution(capacity)
    
    return capacity, nil
}

// calculateHourlyDistribution spreads volume across optimal hours
func (va *VolumeAnalyzer) calculateHourlyDistribution(capacity *VolumeCapacity) [24]int {
    
    distribution := [24]int{}
    remaining := capacity.RemainingToday
    
    // Get historical engagement by hour
    hourlyEngagement := va.db.GetHourlyEngagementRates(context.Background())
    
    // Weight distribution by engagement (send more in high-engagement hours)
    totalWeight := 0.0
    for _, rate := range hourlyEngagement {
        totalWeight += rate
    }
    
    for hour := 0; hour < 24; hour++ {
        if hour >= 6 && hour <= 22 { // Only send 6 AM - 10 PM
            weight := hourlyEngagement[hour] / totalWeight
            distribution[hour] = int(float64(remaining) * weight)
        }
    }
    
    return distribution
}
```

### 2.2 Audience Analysis

```go
// audience_analyzer.go

package plan

type AudienceAnalyzer struct {
    db           *Database
    segmentStore *SegmentStore
    profileStore *ProfileStore
}

// AudienceInsights contains aggregated audience understanding
type AudienceInsights struct {
    // Total universe
    TotalSubscribers        int
    ActiveSubscribers       int       // Engaged in last 90 days
    
    // Segments with performance metrics
    Segments                []SegmentInsight
    
    // Engagement distribution
    HighEngagement          int       // Score 70-100
    MediumEngagement        int       // Score 40-70
    LowEngagement           int       // Score 0-40
    AtRisk                  int       // Declining engagement
    
    // Value distribution
    HighValue               int       // Top 10% LTV
    MediumValue             int       // Middle 60%
    LowValue                int       // Bottom 30%
    
    // Sendable today (not recently emailed, not suppressed)
    SendableCount           int
    
    // By frequency tolerance
    ReadyForEmail           int       // Past their optimal frequency interval
    RecentlyEmailed         int       // Within frequency limit
    
    // By email domain
    DomainDistribution      map[string]DomainInsight
}

type SegmentInsight struct {
    SegmentID               string
    SegmentName             string
    SubscriberCount         int
    SendableCount           int       // Ready to receive email
    
    // Performance metrics
    AvgOpenRate             float64
    AvgClickRate            float64
    AvgConversionRate       float64
    AvgRevenue              float64   // Per email
    
    // Best performing
    BestOffer               string
    BestOfferRevenue        float64
    BestSendHour            int
    BestSendDay             string
    
    // Risk assessment
    AvgChurnRisk            float64
    AvgEngagementScore      float64
    
    // Recommendation
    RecommendedVolume       int
    RecommendedOffer        string
    Priority                string    // "high", "medium", "low"
}

type DomainInsight struct {
    Domain                  string    // "gmail.com", "yahoo.com"
    SubscriberCount         int
    AvgOpenRate             float64
    AvgDeliveryRate         float64
    BestSendHours           []int
    RecentDeliverability    string    // "good", "moderate", "poor"
}

// AnalyzeAudience builds comprehensive audience understanding
func (aa *AudienceAnalyzer) AnalyzeAudience(ctx context.Context) (*AudienceInsights, error) {
    
    insights := &AudienceInsights{
        DomainDistribution: make(map[string]DomainInsight),
    }
    
    // Get total counts
    insights.TotalSubscribers = aa.db.GetTotalSubscribers(ctx)
    insights.ActiveSubscribers = aa.db.GetActiveSubscribers(ctx, 90)
    
    // Get engagement distribution
    engagementDist := aa.db.GetEngagementDistribution(ctx)
    insights.HighEngagement = engagementDist["high"]
    insights.MediumEngagement = engagementDist["medium"]
    insights.LowEngagement = engagementDist["low"]
    insights.AtRisk = engagementDist["at_risk"]
    
    // Get value distribution
    valueDist := aa.db.GetValueDistribution(ctx)
    insights.HighValue = valueDist["high"]
    insights.MediumValue = valueDist["medium"]
    insights.LowValue = valueDist["low"]
    
    // Calculate sendable (respecting frequency limits)
    insights.SendableCount = aa.calculateSendableCount(ctx)
    insights.ReadyForEmail = aa.getReadyForEmailCount(ctx)
    insights.RecentlyEmailed = insights.ActiveSubscribers - insights.ReadyForEmail
    
    // Analyze each segment
    segments, _ := aa.segmentStore.GetAllSegments(ctx)
    for _, segment := range segments {
        segInsight := aa.analyzeSegment(ctx, segment)
        insights.Segments = append(insights.Segments, segInsight)
    }
    
    // Sort segments by priority (revenue potential)
    sort.Slice(insights.Segments, func(i, j int) bool {
        return insights.Segments[i].AvgRevenue * float64(insights.Segments[i].SendableCount) >
               insights.Segments[j].AvgRevenue * float64(insights.Segments[j].SendableCount)
    })
    
    // Analyze by domain
    insights.DomainDistribution = aa.analyzeDomainDistribution(ctx)
    
    return insights, nil
}

// analyzeSegment builds insight for a single segment
func (aa *AudienceAnalyzer) analyzeSegment(
    ctx context.Context,
    segment *Segment,
) SegmentInsight {
    
    insight := SegmentInsight{
        SegmentID:   segment.ID,
        SegmentName: segment.Name,
    }
    
    // Get counts
    insight.SubscriberCount = aa.db.GetSegmentCount(ctx, segment.ID)
    insight.SendableCount = aa.db.GetSegmentSendableCount(ctx, segment.ID)
    
    // Get performance metrics (last 30 days)
    perf := aa.db.GetSegmentPerformance(ctx, segment.ID, 30)
    insight.AvgOpenRate = perf.OpenRate
    insight.AvgClickRate = perf.ClickRate
    insight.AvgConversionRate = perf.ConversionRate
    insight.AvgRevenue = perf.RevenuePerEmail
    
    // Get best performing offer for this segment
    bestOffer := aa.db.GetBestOfferForSegment(ctx, segment.ID)
    if bestOffer != nil {
        insight.BestOffer = bestOffer.Name
        insight.BestOfferRevenue = bestOffer.RevenuePerEmail
    }
    
    // Get best send timing
    timing := aa.db.GetBestTimingForSegment(ctx, segment.ID)
    insight.BestSendHour = timing.BestHour
    insight.BestSendDay = timing.BestDay
    
    // Get risk metrics
    risk := aa.db.GetSegmentRiskMetrics(ctx, segment.ID)
    insight.AvgChurnRisk = risk.AvgChurnRisk
    insight.AvgEngagementScore = risk.AvgEngagementScore
    
    // Calculate priority
    revenuePotential := insight.AvgRevenue * float64(insight.SendableCount)
    if revenuePotential > 1000 {
        insight.Priority = "high"
    } else if revenuePotential > 100 {
        insight.Priority = "medium"
    } else {
        insight.Priority = "low"
    }
    
    // Recommend volume (balance opportunity with risk)
    if insight.AvgChurnRisk < 0.2 {
        insight.RecommendedVolume = insight.SendableCount
    } else if insight.AvgChurnRisk < 0.4 {
        insight.RecommendedVolume = int(float64(insight.SendableCount) * 0.7)
    } else {
        insight.RecommendedVolume = int(float64(insight.SendableCount) * 0.5)
    }
    
    insight.RecommendedOffer = insight.BestOffer
    
    return insight
}
```

### 2.3 Offer Performance Analysis

```go
// offer_analyzer.go

package plan

type OfferAnalyzer struct {
    db            *Database
    revenueStore  *RevenueStore
}

// OfferPerformanceInsights contains offer analysis
type OfferPerformanceInsights struct {
    // Available offers
    ActiveOffers           []OfferInsight
    
    // By category
    CategoryPerformance    map[string]*CategoryPerformance
    
    // Recommendations
    TopPerformingOffers    []string
    RisingOffers           []string   // Improving performance
    DecliningOffers        []string   // Declining performance
    
    // Fatigue analysis
    OverexposedOffers      []string   // Sent too much recently
    FreshOffers            []string   // Not sent recently
}

type OfferInsight struct {
    OfferID                string
    OfferName              string
    Category               string
    Payout                 float64
    
    // Performance metrics (30 days)
    TotalSends             int
    TotalClicks            int
    TotalConversions       int
    TotalRevenue           float64
    
    // Calculated metrics
    ClickRate              float64
    ConversionRate         float64
    EPC                    float64   // Earnings per click
    EPM                    float64   // Earnings per thousand sends
    ROI                    float64
    
    // Trend
    PerformanceTrend       string    // "improving", "stable", "declining"
    TrendPercentage        float64   // % change
    
    // Audience affinity
    BestSegments           []string  // Segments where this offer performs best
    WorstSegments          []string
    
    // Fatigue score
    FatigueScore           float64   // 0-1, higher = more fatigued
    DaysSinceLastSend      int
    
    // Recommendation
    RecommendedForToday    bool
    RecommendedVolume      int
    RecommendationReason   string
}

type CategoryPerformance struct {
    Category               string
    AvgEPC                 float64
    AvgConversionRate      float64
    TotalRevenue30d        float64
    BestOffer              string
    AudienceSize           int       // Subscribers who respond to this category
}

// AnalyzeOffers builds comprehensive offer understanding
func (oa *OfferAnalyzer) AnalyzeOffers(ctx context.Context) (*OfferPerformanceInsights, error) {
    
    insights := &OfferPerformanceInsights{
        CategoryPerformance: make(map[string]*CategoryPerformance),
    }
    
    // Get all active offers
    offers, _ := oa.db.GetActiveOffers(ctx)
    
    for _, offer := range offers {
        offerInsight := oa.analyzeOffer(ctx, offer)
        insights.ActiveOffers = append(insights.ActiveOffers, offerInsight)
        
        // Track top performers
        if offerInsight.EPC > 0.50 && offerInsight.PerformanceTrend != "declining" {
            insights.TopPerformingOffers = append(insights.TopPerformingOffers, offer.ID)
        }
        
        // Track rising offers
        if offerInsight.PerformanceTrend == "improving" && offerInsight.TrendPercentage > 10 {
            insights.RisingOffers = append(insights.RisingOffers, offer.ID)
        }
        
        // Track declining offers
        if offerInsight.PerformanceTrend == "declining" && offerInsight.TrendPercentage < -15 {
            insights.DecliningOffers = append(insights.DecliningOffers, offer.ID)
        }
        
        // Track fatigue
        if offerInsight.FatigueScore > 0.7 {
            insights.OverexposedOffers = append(insights.OverexposedOffers, offer.ID)
        }
        if offerInsight.DaysSinceLastSend > 7 {
            insights.FreshOffers = append(insights.FreshOffers, offer.ID)
        }
    }
    
    // Sort by EPC
    sort.Slice(insights.ActiveOffers, func(i, j int) bool {
        return insights.ActiveOffers[i].EPC > insights.ActiveOffers[j].EPC
    })
    
    // Analyze by category
    insights.CategoryPerformance = oa.analyzeCategoryPerformance(ctx, offers)
    
    return insights, nil
}

// analyzeOffer builds insight for a single offer
func (oa *OfferAnalyzer) analyzeOffer(ctx context.Context, offer *Offer) OfferInsight {
    
    insight := OfferInsight{
        OfferID:   offer.ID,
        OfferName: offer.Name,
        Category:  offer.Category,
        Payout:    offer.Payout,
    }
    
    // Get 30-day performance
    perf := oa.db.GetOfferPerformance(ctx, offer.ID, 30)
    insight.TotalSends = perf.Sends
    insight.TotalClicks = perf.Clicks
    insight.TotalConversions = perf.Conversions
    insight.TotalRevenue = perf.Revenue
    
    // Calculate rates
    if perf.Sends > 0 {
        insight.ClickRate = float64(perf.Clicks) / float64(perf.Sends)
        insight.EPM = (perf.Revenue / float64(perf.Sends)) * 1000
    }
    if perf.Clicks > 0 {
        insight.ConversionRate = float64(perf.Conversions) / float64(perf.Clicks)
        insight.EPC = perf.Revenue / float64(perf.Clicks)
    }
    
    // Calculate trend (compare last 7 days to previous 7 days)
    recent := oa.db.GetOfferPerformance(ctx, offer.ID, 7)
    previous := oa.db.GetOfferPerformanceRange(ctx, offer.ID, 14, 7)
    
    if previous.Revenue > 0 {
        change := (recent.Revenue - previous.Revenue) / previous.Revenue * 100
        insight.TrendPercentage = change
        
        if change > 5 {
            insight.PerformanceTrend = "improving"
        } else if change < -5 {
            insight.PerformanceTrend = "declining"
        } else {
            insight.PerformanceTrend = "stable"
        }
    }
    
    // Get best segments
    insight.BestSegments = oa.db.GetBestSegmentsForOffer(ctx, offer.ID, 3)
    insight.WorstSegments = oa.db.GetWorstSegmentsForOffer(ctx, offer.ID, 3)
    
    // Calculate fatigue
    insight.DaysSinceLastSend = oa.db.GetDaysSinceOfferSent(ctx, offer.ID)
    recentExposure := oa.db.GetOfferExposureRate(ctx, offer.ID, 7)
    insight.FatigueScore = math.Min(1.0, recentExposure * 2)
    
    // Recommendation
    insight.RecommendedForToday = insight.EPC > 0.20 && 
                                   insight.PerformanceTrend != "declining" &&
                                   insight.FatigueScore < 0.8
    
    if insight.RecommendedForToday {
        // Recommend volume based on performance
        baseVolume := 10000
        if insight.EPC > 0.50 {
            insight.RecommendedVolume = baseVolume * 3
        } else if insight.EPC > 0.30 {
            insight.RecommendedVolume = baseVolume * 2
        } else {
            insight.RecommendedVolume = baseVolume
        }
        
        // Reduce if fatigued
        insight.RecommendedVolume = int(float64(insight.RecommendedVolume) * (1 - insight.FatigueScore))
        
        insight.RecommendationReason = fmt.Sprintf(
            "EPC $%.2f, %s trend, fatigue %.0f%%",
            insight.EPC, insight.PerformanceTrend, insight.FatigueScore*100,
        )
    }
    
    return insight
}
```

### 2.4 Timing & Content Analysis

```go
// timing_analyzer.go

package plan

type TimingAnalyzer struct {
    db *Database
}

// TimingInsights contains send time analysis
type TimingInsights struct {
    // Hourly performance
    HourlyPerformance      [24]HourPerformance
    
    // Best times
    BestHours              []int     // Top 3 performing hours
    WorstHours             []int     // Bottom 3 performing hours
    
    // Day of week
    DayPerformance         [7]DayPerformance
    BestDays               []string
    
    // Today's recommendation
    TodayRecommendation    TodayTiming
}

type HourPerformance struct {
    Hour                   int
    AvgOpenRate            float64
    AvgClickRate           float64
    AvgRevenue             float64
    VolumeHandled          int       // Historical volume at this hour
    DeliverabilityScore    float64   // ISP acceptance at this hour
    Recommended            bool
    RecommendedVolume      int
}

type DayPerformance struct {
    Day                    string
    AvgOpenRate            float64
    AvgClickRate           float64
    AvgRevenue             float64
}

type TodayTiming struct {
    IsGoodDay              bool
    DayQualityScore        float64   // 0-100
    OptimalWindows         []TimeWindow
    AvoidWindows           []TimeWindow
}

type TimeWindow struct {
    Start                  int       // Hour
    End                    int       // Hour
    Reason                 string
    ExpectedOpenRate       float64
    RecommendedVolume      int
}

// AnalyzeTiming builds timing insights
func (ta *TimingAnalyzer) AnalyzeTiming(ctx context.Context) (*TimingInsights, error) {
    
    insights := &TimingInsights{}
    
    // Analyze hourly performance
    for hour := 0; hour < 24; hour++ {
        perf := ta.db.GetHourlyPerformance(ctx, hour, 30)
        insights.HourlyPerformance[hour] = HourPerformance{
            Hour:                hour,
            AvgOpenRate:         perf.OpenRate,
            AvgClickRate:        perf.ClickRate,
            AvgRevenue:          perf.RevenuePerEmail,
            VolumeHandled:       perf.AvgVolume,
            DeliverabilityScore: perf.DeliveryRate * 100,
        }
    }
    
    // Find best/worst hours
    insights.BestHours = ta.findBestHours(insights.HourlyPerformance[:])
    insights.WorstHours = ta.findWorstHours(insights.HourlyPerformance[:])
    
    // Mark recommendations
    for _, hour := range insights.BestHours {
        insights.HourlyPerformance[hour].Recommended = true
        insights.HourlyPerformance[hour].RecommendedVolume = 
            insights.HourlyPerformance[hour].VolumeHandled * 2
    }
    
    // Analyze day of week
    days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
    for i, day := range days {
        perf := ta.db.GetDayPerformance(ctx, i, 30)
        insights.DayPerformance[i] = DayPerformance{
            Day:         day,
            AvgOpenRate: perf.OpenRate,
            AvgClickRate: perf.ClickRate,
            AvgRevenue:  perf.RevenuePerEmail,
        }
    }
    
    insights.BestDays = ta.findBestDays(insights.DayPerformance[:])
    
    // Today's recommendation
    today := time.Now().Weekday()
    todayPerf := insights.DayPerformance[today]
    
    insights.TodayRecommendation = TodayTiming{
        IsGoodDay:       contains(insights.BestDays, todayPerf.Day),
        DayQualityScore: todayPerf.AvgRevenue / insights.getAvgDayRevenue() * 100,
    }
    
    // Build time windows
    insights.TodayRecommendation.OptimalWindows = ta.buildOptimalWindows(insights)
    insights.TodayRecommendation.AvoidWindows = ta.buildAvoidWindows(insights)
    
    return insights, nil
}

// buildOptimalWindows creates recommended send windows
func (ta *TimingAnalyzer) buildOptimalWindows(insights *TimingInsights) []TimeWindow {
    
    windows := []TimeWindow{}
    
    // Morning window (high engagement typically)
    morningStart := 7
    morningEnd := 10
    morningAvgOpen := ta.avgOpenForRange(insights.HourlyPerformance[:], morningStart, morningEnd)
    
    windows = append(windows, TimeWindow{
        Start:            morningStart,
        End:              morningEnd,
        Reason:           "Peak morning engagement",
        ExpectedOpenRate: morningAvgOpen,
        RecommendedVolume: 30, // % of daily volume
    })
    
    // Midday window
    middayStart := 11
    middayEnd := 14
    middayAvgOpen := ta.avgOpenForRange(insights.HourlyPerformance[:], middayStart, middayEnd)
    
    windows = append(windows, TimeWindow{
        Start:            middayStart,
        End:              middayEnd,
        Reason:           "Lunch break engagement",
        ExpectedOpenRate: middayAvgOpen,
        RecommendedVolume: 25,
    })
    
    // Evening window
    eveningStart := 18
    eveningEnd := 21
    eveningAvgOpen := ta.avgOpenForRange(insights.HourlyPerformance[:], eveningStart, eveningEnd)
    
    windows = append(windows, TimeWindow{
        Start:            eveningStart,
        End:              eveningEnd,
        Reason:           "Evening relaxation",
        ExpectedOpenRate: eveningAvgOpen,
        RecommendedVolume: 25,
    })
    
    return windows
}
```

---

## 3. Sending Plan Generator

### 3.1 Plan Generator Core

```go
// plan_generator.go

package plan

type SendingPlanGenerator struct {
    volumeAnalyzer   *VolumeAnalyzer
    audienceAnalyzer *AudienceAnalyzer
    offerAnalyzer    *OfferAnalyzer
    timingAnalyzer   *TimingAnalyzer
    revenuePredictor *RevenuePredictor
}

// SendingPlan represents a complete daily sending plan
type SendingPlan struct {
    // Metadata
    PlanID               string
    GeneratedAt          time.Time
    PlanDate             time.Time
    Status               string      // "draft", "approved", "executing", "completed"
    
    // Input summary
    InputSummary         PlanInputSummary
    
    // Plan options by time period
    MorningPlan          *TimePeriodPlan
    FirstHalfPlan        *TimePeriodPlan
    FullDayPlan          *TimePeriodPlan
    
    // Overall projections
    TotalProjectedRevenue  float64
    TotalProjectedSends    int
    TotalProjectedOpens    int
    TotalProjectedClicks   int
    TotalProjectedConversions int
    
    // Risk assessment
    RiskAssessment       PlanRiskAssessment
    
    // AI explanation
    AIExplanation        string
    KeyInsights          []string
    Recommendations      []string
}

// PlanInputSummary summarizes all inputs considered
type PlanInputSummary struct {
    // Volume
    DailyCapacity        int
    ESPsAvailable        []string
    WarmupConstraints    int
    
    // Audience
    TotalSendable        int
    HighValueSendable    int
    SegmentsAnalyzed     int
    
    // Offers
    ActiveOffers         int
    TopPerformingOffers  int
    
    // Timing
    TodayQuality         string    // "excellent", "good", "average", "poor"
    OptimalWindows       int
}

// TimePeriodPlan represents a plan for a specific time window
type TimePeriodPlan struct {
    // Period definition
    PeriodName           string    // "Morning", "First Half", "Full Day"
    StartHour            int
    EndHour              int
    
    // Volume
    TotalVolume          int
    VolumeByHour         map[int]int
    
    // Segment allocations
    SegmentPlans         []SegmentPlanItem
    
    // Offer allocations
    OfferPlans           []OfferPlanItem
    
    // Projections
    ProjectedRevenue     float64
    ProjectedOpenRate    float64
    ProjectedClickRate   float64
    ProjectedCVR         float64
    RevenuePerEmail      float64
    
    // Confidence
    Confidence           float64   // 0-1
    ConfidenceFactors    []string
    
    // Approval
    ApprovalStatus       string    // "pending", "approved", "rejected"
    ApprovedBy           string
    ApprovedAt           *time.Time
}

// SegmentPlanItem shows what to send to each segment
type SegmentPlanItem struct {
    SegmentID            string
    SegmentName          string
    SegmentSize          int
    
    // Allocation
    PlannedVolume        int
    PercentOfSegment     float64
    
    // Assignment
    AssignedOffer        string
    AssignedOfferName    string
    SendHours            []int     // Which hours to send
    
    // Expected performance
    ExpectedOpenRate     float64
    ExpectedClickRate    float64
    ExpectedRevenue      float64
    
    // Rationale
    SelectionRationale   string
}

// OfferPlanItem shows volume per offer
type OfferPlanItem struct {
    OfferID              string
    OfferName            string
    Category             string
    
    // Volume
    PlannedVolume        int
    TargetSegments       []string
    
    // Expected performance
    ExpectedEPC          float64
    ExpectedRevenue      float64
    
    // Rationale
    SelectionRationale   string
}

// GeneratePlan creates a complete sending plan
func (spg *SendingPlanGenerator) GeneratePlan(
    ctx context.Context,
    planDate time.Time,
) (*SendingPlan, error) {
    
    log.Info().Time("date", planDate).Msg("Generating sending plan")
    
    // Gather all inputs
    volume, err := spg.volumeAnalyzer.AnalyzeCapacity(ctx)
    if err != nil {
        return nil, fmt.Errorf("volume analysis: %w", err)
    }
    
    audience, err := spg.audienceAnalyzer.AnalyzeAudience(ctx)
    if err != nil {
        return nil, fmt.Errorf("audience analysis: %w", err)
    }
    
    offers, err := spg.offerAnalyzer.AnalyzeOffers(ctx)
    if err != nil {
        return nil, fmt.Errorf("offer analysis: %w", err)
    }
    
    timing, err := spg.timingAnalyzer.AnalyzeTiming(ctx)
    if err != nil {
        return nil, fmt.Errorf("timing analysis: %w", err)
    }
    
    // Build plan
    plan := &SendingPlan{
        PlanID:      generatePlanID(),
        GeneratedAt: time.Now(),
        PlanDate:    planDate,
        Status:      "draft",
    }
    
    // Summarize inputs
    plan.InputSummary = spg.buildInputSummary(volume, audience, offers, timing)
    
    // Generate plans for each time period
    plan.MorningPlan = spg.generateTimePeriodPlan(ctx, "Morning", 6, 12, 
        volume, audience, offers, timing)
    
    plan.FirstHalfPlan = spg.generateTimePeriodPlan(ctx, "First Half", 6, 15,
        volume, audience, offers, timing)
    
    plan.FullDayPlan = spg.generateTimePeriodPlan(ctx, "Full Day", 6, 22,
        volume, audience, offers, timing)
    
    // Calculate total projections
    plan.TotalProjectedRevenue = plan.FullDayPlan.ProjectedRevenue
    plan.TotalProjectedSends = plan.FullDayPlan.TotalVolume
    
    // Risk assessment
    plan.RiskAssessment = spg.assessRisk(plan, audience, offers)
    
    // Generate AI explanation
    plan.AIExplanation = spg.generateExplanation(plan, volume, audience, offers, timing)
    plan.KeyInsights = spg.extractKeyInsights(plan)
    plan.Recommendations = spg.generateRecommendations(plan)
    
    return plan, nil
}

// generateTimePeriodPlan creates a plan for a specific time window
func (spg *SendingPlanGenerator) generateTimePeriodPlan(
    ctx context.Context,
    periodName string,
    startHour, endHour int,
    volume *VolumeCapacity,
    audience *AudienceInsights,
    offers *OfferPerformanceInsights,
    timing *TimingInsights,
) *TimePeriodPlan {
    
    plan := &TimePeriodPlan{
        PeriodName:     periodName,
        StartHour:      startHour,
        EndHour:        endHour,
        VolumeByHour:   make(map[int]int),
        ApprovalStatus: "pending",
    }
    
    // Calculate available volume for this period
    periodVolume := 0
    for hour := startHour; hour < endHour; hour++ {
        hourlyVol := volume.HourlyRecommendation[hour]
        plan.VolumeByHour[hour] = hourlyVol
        periodVolume += hourlyVol
    }
    
    // Cap at sendable audience
    if periodVolume > audience.SendableCount {
        periodVolume = audience.SendableCount
        // Redistribute
        for hour := startHour; hour < endHour; hour++ {
            plan.VolumeByHour[hour] = periodVolume / (endHour - startHour)
        }
    }
    
    plan.TotalVolume = periodVolume
    
    // Allocate to segments
    remainingVolume := periodVolume
    for _, segment := range audience.Segments {
        if remainingVolume <= 0 {
            break
        }
        
        if segment.Priority == "low" && remainingVolume < periodVolume/2 {
            continue // Skip low priority if running low
        }
        
        // Calculate allocation for this segment
        allocation := min(segment.RecommendedVolume, remainingVolume)
        allocation = min(allocation, segment.SendableCount)
        
        if allocation > 0 {
            // Find best offer for this segment
            bestOffer := spg.findBestOfferForSegment(segment, offers)
            
            // Determine send hours
            sendHours := spg.determineSendHours(segment, timing, startHour, endHour)
            
            // Predict performance
            prediction := spg.revenuePredictor.PredictForSegmentOffer(
                ctx, segment.SegmentID, bestOffer.OfferID,
            )
            
            segmentPlan := SegmentPlanItem{
                SegmentID:          segment.SegmentID,
                SegmentName:        segment.SegmentName,
                SegmentSize:        segment.SendableCount,
                PlannedVolume:      allocation,
                PercentOfSegment:   float64(allocation) / float64(segment.SendableCount) * 100,
                AssignedOffer:      bestOffer.OfferID,
                AssignedOfferName:  bestOffer.OfferName,
                SendHours:          sendHours,
                ExpectedOpenRate:   prediction.OpenRate,
                ExpectedClickRate:  prediction.ClickRate,
                ExpectedRevenue:    prediction.Revenue,
                SelectionRationale: fmt.Sprintf(
                    "Segment avg revenue $%.3f, offer EPC $%.2f, best hours %v",
                    segment.AvgRevenue, bestOffer.EPC, sendHours,
                ),
            }
            
            plan.SegmentPlans = append(plan.SegmentPlans, segmentPlan)
            remainingVolume -= allocation
        }
    }
    
    // Aggregate offer allocations
    offerVolumes := make(map[string]int)
    offerSegments := make(map[string][]string)
    
    for _, sp := range plan.SegmentPlans {
        offerVolumes[sp.AssignedOffer] += sp.PlannedVolume
        offerSegments[sp.AssignedOffer] = append(offerSegments[sp.AssignedOffer], sp.SegmentName)
    }
    
    for offerID, vol := range offerVolumes {
        offer := spg.getOfferByID(offers, offerID)
        plan.OfferPlans = append(plan.OfferPlans, OfferPlanItem{
            OfferID:            offerID,
            OfferName:          offer.OfferName,
            Category:           offer.Category,
            PlannedVolume:      vol,
            TargetSegments:     offerSegments[offerID],
            ExpectedEPC:        offer.EPC,
            ExpectedRevenue:    float64(vol) * offer.EPM / 1000,
            SelectionRationale: fmt.Sprintf("EPC $%.2f, %s trend", offer.EPC, offer.PerformanceTrend),
        })
    }
    
    // Calculate projections
    plan.ProjectedRevenue = 0
    totalExpectedOpens := 0.0
    totalExpectedClicks := 0.0
    
    for _, sp := range plan.SegmentPlans {
        plan.ProjectedRevenue += sp.ExpectedRevenue
        totalExpectedOpens += float64(sp.PlannedVolume) * sp.ExpectedOpenRate
        totalExpectedClicks += float64(sp.PlannedVolume) * sp.ExpectedClickRate
    }
    
    plan.ProjectedOpenRate = totalExpectedOpens / float64(plan.TotalVolume)
    plan.ProjectedClickRate = totalExpectedClicks / float64(plan.TotalVolume)
    plan.RevenuePerEmail = plan.ProjectedRevenue / float64(plan.TotalVolume)
    
    // Calculate confidence
    plan.Confidence = spg.calculateConfidence(plan, audience)
    plan.ConfidenceFactors = spg.getConfidenceFactors(plan, audience)
    
    return plan
}
```

### 3.2 AI Explanation Generator

```go
// explanation_generator.go

package plan

// generateExplanation creates human-readable plan explanation
func (spg *SendingPlanGenerator) generateExplanation(
    plan *SendingPlan,
    volume *VolumeCapacity,
    audience *AudienceInsights,
    offers *OfferPerformanceInsights,
    timing *TimingInsights,
) string {
    
    var sb strings.Builder
    
    sb.WriteString("## Sending Plan Analysis\n\n")
    
    // Volume context
    sb.WriteString(fmt.Sprintf("### Capacity\n"))
    sb.WriteString(fmt.Sprintf("You have **%s emails** available to send today. ",
        formatNumber(volume.RemainingToday)))
    sb.WriteString(fmt.Sprintf("Your sendable audience is **%s subscribers**.\n\n",
        formatNumber(audience.SendableCount)))
    
    // Audience insights
    sb.WriteString("### Audience Insights\n")
    sb.WriteString(fmt.Sprintf("- **%s** high-engagement subscribers ready (score 70+)\n",
        formatNumber(audience.HighEngagement)))
    sb.WriteString(fmt.Sprintf("- **%s** high-value subscribers (top 10%% LTV)\n",
        formatNumber(audience.HighValue)))
    sb.WriteString(fmt.Sprintf("- **%s** at-risk subscribers (declining engagement)\n\n",
        formatNumber(audience.AtRisk)))
    
    // Offer insights
    sb.WriteString("### Offer Performance\n")
    if len(offers.TopPerformingOffers) > 0 {
        sb.WriteString(fmt.Sprintf("Top performers: "))
        topOffers := []string{}
        for _, id := range offers.TopPerformingOffers[:min(3, len(offers.TopPerformingOffers))] {
            offer := spg.getOfferByID(offers, id)
            topOffers = append(topOffers, fmt.Sprintf("**%s** ($%.2f EPC)", offer.OfferName, offer.EPC))
        }
        sb.WriteString(strings.Join(topOffers, ", "))
        sb.WriteString("\n")
    }
    if len(offers.OverexposedOffers) > 0 {
        sb.WriteString(fmt.Sprintf("⚠️ **%d offers** showing fatigue - reduced allocation\n",
            len(offers.OverexposedOffers)))
    }
    sb.WriteString("\n")
    
    // Timing insights
    sb.WriteString("### Timing Analysis\n")
    sb.WriteString(fmt.Sprintf("Today (%s) is a **%s** day for sending.\n",
        time.Now().Weekday().String(),
        getTodayQuality(timing)))
    sb.WriteString(fmt.Sprintf("Best hours: %v\n\n", timing.BestHours))
    
    // Plan summary
    sb.WriteString("### Plan Summary\n")
    sb.WriteString(fmt.Sprintf("| Period | Volume | Projected Revenue | RPE |\n"))
    sb.WriteString(fmt.Sprintf("|--------|--------|-------------------|-----|\n"))
    sb.WriteString(fmt.Sprintf("| Morning (6AM-12PM) | %s | $%s | $%.3f |\n",
        formatNumber(plan.MorningPlan.TotalVolume),
        formatCurrency(plan.MorningPlan.ProjectedRevenue),
        plan.MorningPlan.RevenuePerEmail))
    sb.WriteString(fmt.Sprintf("| First Half (6AM-3PM) | %s | $%s | $%.3f |\n",
        formatNumber(plan.FirstHalfPlan.TotalVolume),
        formatCurrency(plan.FirstHalfPlan.ProjectedRevenue),
        plan.FirstHalfPlan.RevenuePerEmail))
    sb.WriteString(fmt.Sprintf("| Full Day (6AM-10PM) | %s | $%s | $%.3f |\n",
        formatNumber(plan.FullDayPlan.TotalVolume),
        formatCurrency(plan.FullDayPlan.ProjectedRevenue),
        plan.FullDayPlan.RevenuePerEmail))
    
    return sb.String()
}

// extractKeyInsights pulls out the most important findings
func (spg *SendingPlanGenerator) extractKeyInsights(plan *SendingPlan) []string {
    
    insights := []string{}
    
    // Revenue opportunity
    insights = append(insights, fmt.Sprintf(
        "💰 Full day plan projects **$%s revenue** from %s emails",
        formatCurrency(plan.FullDayPlan.ProjectedRevenue),
        formatNumber(plan.FullDayPlan.TotalVolume),
    ))
    
    // Top segment
    if len(plan.FullDayPlan.SegmentPlans) > 0 {
        topSegment := plan.FullDayPlan.SegmentPlans[0]
        insights = append(insights, fmt.Sprintf(
            "🎯 **%s** segment has highest revenue potential ($%.2f/email)",
            topSegment.SegmentName,
            topSegment.ExpectedRevenue/float64(topSegment.PlannedVolume),
        ))
    }
    
    // Top offer
    if len(plan.FullDayPlan.OfferPlans) > 0 {
        topOffer := plan.FullDayPlan.OfferPlans[0]
        insights = append(insights, fmt.Sprintf(
            "⭐ **%s** is the top offer with $%.2f EPC",
            topOffer.OfferName,
            topOffer.ExpectedEPC,
        ))
    }
    
    // Timing
    insights = append(insights, fmt.Sprintf(
        "⏰ Best send window: **%d:00 - %d:00** based on historical engagement",
        plan.MorningPlan.StartHour,
        plan.MorningPlan.EndHour,
    ))
    
    // Confidence
    confidenceLevel := "high"
    if plan.FullDayPlan.Confidence < 0.7 {
        confidenceLevel = "moderate"
    }
    if plan.FullDayPlan.Confidence < 0.5 {
        confidenceLevel = "low"
    }
    insights = append(insights, fmt.Sprintf(
        "📊 Plan confidence: **%s** (%.0f%%)",
        confidenceLevel,
        plan.FullDayPlan.Confidence*100,
    ))
    
    return insights
}

// generateRecommendations creates actionable recommendations
func (spg *SendingPlanGenerator) generateRecommendations(plan *SendingPlan) []string {
    
    recs := []string{}
    
    // Compare periods
    morningRPE := plan.MorningPlan.RevenuePerEmail
    fullDayRPE := plan.FullDayPlan.RevenuePerEmail
    
    if morningRPE > fullDayRPE * 1.1 {
        recs = append(recs, 
            "📈 Morning sends have **higher RPE** - consider concentrating volume in AM hours")
    }
    
    // Risk assessment
    if plan.RiskAssessment.OverallRisk == "high" {
        recs = append(recs,
            "⚠️ High risk detected - consider starting with **Morning Plan** to monitor performance")
    }
    
    // Offer diversity
    if len(plan.FullDayPlan.OfferPlans) < 3 {
        recs = append(recs,
            "🔄 Consider testing more offers to diversify revenue sources")
    }
    
    // Segment coverage
    totalSegments := len(plan.InputSummary.SegmentsAnalyzed)
    coveredSegments := len(plan.FullDayPlan.SegmentPlans)
    if coveredSegments < totalSegments/2 {
        recs = append(recs, fmt.Sprintf(
            "📋 Only %d/%d segments included - low-volume segments skipped for efficiency",
            coveredSegments, totalSegments,
        ))
    }
    
    return recs
}
```

---

## 4. API Endpoints

### 4.1 Plan Generation & Approval API

```yaml
openapi: 3.0.0
info:
  title: Sending Plan API
  version: 1.0.0

paths:
  /api/v1/sending-plans/generate:
    post:
      summary: Generate a new sending plan
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                plan_date:
                  type: string
                  format: date
                  description: Date to generate plan for (default today)
      responses:
        '200':
          description: Generated sending plan
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/SendingPlan'

  /api/v1/sending-plans/{planId}:
    get:
      summary: Get sending plan details
      parameters:
        - name: planId
          in: path
          required: true
          schema:
            type: string
      responses:
        '200':
          description: Sending plan
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/SendingPlan'

  /api/v1/sending-plans/{planId}/approve:
    post:
      summary: Approve a time period plan
      parameters:
        - name: planId
          in: path
          required: true
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required:
                - period
              properties:
                period:
                  type: string
                  enum: [morning, first_half, full_day]
                modifications:
                  type: object
                  description: Optional modifications before approval
                  properties:
                    volume_adjustment:
                      type: number
                      description: Multiply volume by this factor
                    exclude_segments:
                      type: array
                      items:
                        type: string
                    exclude_offers:
                      type: array
                      items:
                        type: string
      responses:
        '200':
          description: Plan approved and queued for execution
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                  execution_id:
                    type: string
                  scheduled_volume:
                    type: integer
                  projected_revenue:
                    type: number

  /api/v1/sending-plans/{planId}/execution-status:
    get:
      summary: Get execution status of approved plan
      parameters:
        - name: planId
          in: path
          required: true
      responses:
        '200':
          description: Execution status
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                    enum: [pending, executing, completed, paused]
                  progress:
                    type: object
                    properties:
                      sent:
                        type: integer
                      remaining:
                        type: integer
                      percent_complete:
                        type: number
                  performance:
                    type: object
                    properties:
                      actual_revenue:
                        type: number
                      actual_open_rate:
                        type: number
                      vs_projected:
                        type: number
```

---

## 5. Dashboard UI

### 5.1 Sending Plan Dashboard

```typescript
// SendingPlanDashboard.tsx

interface SendingPlanDashboardProps {
  orgId: string;
}

export const SendingPlanDashboard: React.FC<SendingPlanDashboardProps> = ({
  orgId
}) => {
  const [plan, setPlan] = useState<SendingPlan | null>(null);
  const [generating, setGenerating] = useState(false);
  const [selectedPeriod, setSelectedPeriod] = useState<'morning' | 'first_half' | 'full_day'>('full_day');
  
  const generatePlan = async () => {
    setGenerating(true);
    const newPlan = await api.generateSendingPlan(orgId);
    setPlan(newPlan);
    setGenerating(false);
  };
  
  const approvePlan = async (period: string) => {
    await api.approvePlan(plan.planId, period);
    // Refresh
  };
  
  return (
    <div className="sending-plan-dashboard">
      <header className="plan-header">
        <h1>Daily Sending Plan</h1>
        <div className="actions">
          <Button onClick={generatePlan} loading={generating}>
            {plan ? 'Regenerate Plan' : 'Generate Plan'}
          </Button>
        </div>
      </header>
      
      {plan && (
        <>
          {/* Input Summary */}
          <Card className="input-summary">
            <h2>What the AI Analyzed</h2>
            <div className="summary-grid">
              <SummaryItem
                icon="📊"
                label="Daily Capacity"
                value={formatNumber(plan.inputSummary.dailyCapacity)}
              />
              <SummaryItem
                icon="👥"
                label="Sendable Audience"
                value={formatNumber(plan.inputSummary.totalSendable)}
              />
              <SummaryItem
                icon="⭐"
                label="High-Value Ready"
                value={formatNumber(plan.inputSummary.highValueSendable)}
              />
              <SummaryItem
                icon="🎁"
                label="Active Offers"
                value={plan.inputSummary.activeOffers}
              />
              <SummaryItem
                icon="📈"
                label="Top Performers"
                value={plan.inputSummary.topPerformingOffers}
              />
              <SummaryItem
                icon="📅"
                label="Today's Quality"
                value={plan.inputSummary.todayQuality}
                highlight={plan.inputSummary.todayQuality === 'excellent'}
              />
            </div>
          </Card>
          
          {/* AI Insights */}
          <Card className="ai-insights">
            <h2>🤖 AI Insights</h2>
            <div className="insights-list">
              {plan.keyInsights.map((insight, i) => (
                <div key={i} className="insight-item">
                  {insight}
                </div>
              ))}
            </div>
            
            {plan.recommendations.length > 0 && (
              <div className="recommendations">
                <h3>Recommendations</h3>
                <ul>
                  {plan.recommendations.map((rec, i) => (
                    <li key={i}>{rec}</li>
                  ))}
                </ul>
              </div>
            )}
          </Card>
          
          {/* Plan Options */}
          <div className="plan-options">
            <h2>Choose Your Plan</h2>
            <p className="subtitle">Select the time period you want to approve</p>
            
            <div className="period-cards">
              {/* Morning Plan */}
              <PeriodCard
                period={plan.morningPlan}
                selected={selectedPeriod === 'morning'}
                onSelect={() => setSelectedPeriod('morning')}
                onApprove={() => approvePlan('morning')}
              />
              
              {/* First Half Plan */}
              <PeriodCard
                period={plan.firstHalfPlan}
                selected={selectedPeriod === 'first_half'}
                onSelect={() => setSelectedPeriod('first_half')}
                onApprove={() => approvePlan('first_half')}
              />
              
              {/* Full Day Plan */}
              <PeriodCard
                period={plan.fullDayPlan}
                selected={selectedPeriod === 'full_day'}
                onSelect={() => setSelectedPeriod('full_day')}
                onApprove={() => approvePlan('full_day')}
                recommended
              />
            </div>
          </div>
          
          {/* Detailed View */}
          <Card className="detailed-plan">
            <h2>Plan Details: {plan[`${selectedPeriod}Plan`]?.periodName}</h2>
            
            <Tabs>
              <Tab label="By Segment">
                <SegmentPlanTable segments={plan[`${selectedPeriod}Plan`]?.segmentPlans} />
              </Tab>
              <Tab label="By Offer">
                <OfferPlanTable offers={plan[`${selectedPeriod}Plan`]?.offerPlans} />
              </Tab>
              <Tab label="By Hour">
                <HourlyVolumeChart hours={plan[`${selectedPeriod}Plan`]?.volumeByHour} />
              </Tab>
            </Tabs>
          </Card>
          
          {/* Risk Assessment */}
          <Card className="risk-assessment">
            <h2>Risk Assessment</h2>
            <RiskMeter risk={plan.riskAssessment.overallRisk} />
            <div className="risk-factors">
              {plan.riskAssessment.factors.map((factor, i) => (
                <RiskFactor key={i} {...factor} />
              ))}
            </div>
          </Card>
        </>
      )}
    </div>
  );
};

// PeriodCard Component
const PeriodCard: React.FC<{
  period: TimePeriodPlan;
  selected: boolean;
  recommended?: boolean;
  onSelect: () => void;
  onApprove: () => void;
}> = ({ period, selected, recommended, onSelect, onApprove }) => {
  
  return (
    <div 
      className={`period-card ${selected ? 'selected' : ''} ${recommended ? 'recommended' : ''}`}
      onClick={onSelect}
    >
      {recommended && <Badge>Recommended</Badge>}
      
      <h3>{period.periodName}</h3>
      <p className="time-range">{period.startHour}:00 - {period.endHour}:00</p>
      
      <div className="metrics">
        <div className="metric">
          <span className="label">Emails</span>
          <span className="value">{formatNumber(period.totalVolume)}</span>
        </div>
        <div className="metric primary">
          <span className="label">Projected Revenue</span>
          <span className="value">${formatCurrency(period.projectedRevenue)}</span>
        </div>
        <div className="metric">
          <span className="label">Revenue/Email</span>
          <span className="value">${period.revenuePerEmail.toFixed(3)}</span>
        </div>
        <div className="metric">
          <span className="label">Expected Open Rate</span>
          <span className="value">{(period.projectedOpenRate * 100).toFixed(1)}%</span>
        </div>
      </div>
      
      <div className="confidence">
        <span>Confidence: </span>
        <ConfidenceBadge value={period.confidence} />
      </div>
      
      {selected && (
        <div className="approval-actions">
          <Button 
            variant="primary" 
            onClick={(e) => { e.stopPropagation(); onApprove(); }}
          >
            ✓ Approve & Execute
          </Button>
          <Button variant="secondary">
            Modify Before Approval
          </Button>
        </div>
      )}
    </div>
  );
};

// SegmentPlanTable Component
const SegmentPlanTable: React.FC<{ segments: SegmentPlanItem[] }> = ({ segments }) => {
  return (
    <table className="plan-table">
      <thead>
        <tr>
          <th>Segment</th>
          <th>Volume</th>
          <th>Offer</th>
          <th>Send Hours</th>
          <th>Exp. Open Rate</th>
          <th>Exp. Revenue</th>
          <th>Rationale</th>
        </tr>
      </thead>
      <tbody>
        {segments.map((seg, i) => (
          <tr key={i}>
            <td>
              <strong>{seg.segmentName}</strong>
              <br />
              <small>{formatNumber(seg.segmentSize)} subscribers</small>
            </td>
            <td>
              {formatNumber(seg.plannedVolume)}
              <br />
              <small>{seg.percentOfSegment.toFixed(0)}% of segment</small>
            </td>
            <td>{seg.assignedOfferName}</td>
            <td>
              {seg.sendHours.map(h => `${h}:00`).join(', ')}
            </td>
            <td>{(seg.expectedOpenRate * 100).toFixed(1)}%</td>
            <td>${formatCurrency(seg.expectedRevenue)}</td>
            <td><small>{seg.selectionRationale}</small></td>
          </tr>
        ))}
      </tbody>
      <tfoot>
        <tr>
          <td><strong>Total</strong></td>
          <td><strong>{formatNumber(segments.reduce((s, seg) => s + seg.plannedVolume, 0))}</strong></td>
          <td colSpan={3}></td>
          <td><strong>${formatCurrency(segments.reduce((s, seg) => s + seg.expectedRevenue, 0))}</strong></td>
          <td></td>
        </tr>
      </tfoot>
    </table>
  );
};
```

---

## 6. Visual Plan Summary

### 6.1 High-Level Plan View

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         DAILY SENDING PLAN                                      │
│                         February 2, 2026                                        │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  📊 WHAT THE AI ANALYZED                                                        │
│  ════════════════════════                                                       │
│                                                                                 │
│  Capacity          Audience           Offers           Timing                   │
│  ─────────         ────────           ──────           ──────                   │
│  500,000/day       350,000 sendable   12 active        Tuesday = Good Day       │
│  3 ESPs healthy    45,000 high-value  3 top performers Best hours: 7-10 AM     │
│  2 domains warming 28,000 at-risk     2 fatigued       Avoid: 2-4 PM           │
│                                                                                 │
│  💡 KEY INSIGHTS                                                                │
│  ═══════════════                                                                │
│                                                                                 │
│  • Full day plan projects $12,500 revenue from 250,000 emails                  │
│  • "High Engagement" segment has highest ROI ($0.08/email)                     │
│  • TechDeal Premium is top offer with $0.72 EPC                                │
│  • Morning sends have 15% higher open rates than afternoon                     │
│                                                                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  📋 CHOOSE YOUR PLAN                                                            │
│  ═══════════════════                                                            │
│                                                                                 │
│  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐             │
│  │   🌅 MORNING     │  │   🌤️ FIRST HALF  │  │   ☀️ FULL DAY    │ ★ RECOMMENDED│
│  │   6 AM - 12 PM   │  │   6 AM - 3 PM    │  │   6 AM - 10 PM   │             │
│  │                  │  │                  │  │                  │             │
│  │  75,000 emails   │  │  150,000 emails  │  │  250,000 emails  │             │
│  │                  │  │                  │  │                  │             │
│  │  Revenue: $4,200 │  │  Revenue: $7,800 │  │  Revenue: $12,500│             │
│  │  RPE: $0.056     │  │  RPE: $0.052     │  │  RPE: $0.050     │             │
│  │                  │  │                  │  │                  │             │
│  │  Open: 24%       │  │  Open: 22%       │  │  Open: 21%       │             │
│  │  Click: 3.8%     │  │  Click: 3.5%     │  │  Click: 3.2%     │             │
│  │                  │  │                  │  │                  │             │
│  │  Confidence: 85% │  │  Confidence: 82% │  │  Confidence: 78% │             │
│  │                  │  │                  │  │                  │             │
│  │  [  APPROVE  ]   │  │  [  APPROVE  ]   │  │  [  APPROVE  ]   │             │
│  └──────────────────┘  └──────────────────┘  └──────────────────┘             │
│                                                                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  📊 FULL DAY BREAKDOWN                                                          │
│  ══════════════════════                                                         │
│                                                                                 │
│  BY SEGMENT                              BY OFFER                               │
│  ──────────                              ────────                               │
│  High Engagement    80,000  →  $4,800    TechDeal Premium   100,000  $7,200    │
│  Medium Engagement  95,000  →  $4,275    FinanceApp Pro      80,000  $3,600    │
│  Re-engagement      40,000  →  $1,600    HealthSub Daily     50,000  $1,250    │
│  New Subscribers    35,000  →  $1,825    NewOffer Test       20,000    $450    │
│                                                                                 │
│  HOURLY DISTRIBUTION                                                            │
│  ────────────────────                                                           │
│                                                                                 │
│  Volume │                                                                       │
│  40K    │      ████                                                            │
│  30K    │    ████████                    ████                                  │
│  20K    │  ████████████    ████        ████████    ████                        │
│  10K    │████████████████████████    ████████████████████████                  │
│   0     └────────────────────────────────────────────────────                  │
│          6  7  8  9  10 11 12 13 14 15 16 17 18 19 20 21 22                    │
│                                                                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  ⚠️ RISK ASSESSMENT: LOW                                                        │
│                                                                                 │
│  ✓ Volume within daily limits                                                  │
│  ✓ All segments have healthy engagement                                        │
│  ✓ No offers showing severe fatigue                                            │
│  ⚡ 2 domains in warmup - volume capped accordingly                            │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 7. Plan Execution Tracking

### 7.1 Real-Time Execution Monitor

```go
// execution_tracker.go

package plan

type ExecutionTracker struct {
    db            *Database
    revenueTracker *RevenueTracker
}

// ExecutionStatus tracks real-time plan execution
type ExecutionStatus struct {
    PlanID             string
    Period             string
    Status             string    // "executing", "completed", "paused"
    
    // Progress
    PlannedVolume      int
    SentVolume         int
    RemainingVolume    int
    PercentComplete    float64
    
    // Performance vs Plan
    PlannedRevenue     float64
    ActualRevenue      float64
    RevenueVariance    float64   // Actual - Planned
    PerformanceRatio   float64   // Actual / Planned
    
    // Metrics
    ActualOpenRate     float64
    ActualClickRate    float64
    ActualCVR          float64
    
    // By segment (real-time)
    SegmentPerformance []SegmentExecutionStatus
    
    // Timeline
    StartedAt          time.Time
    EstimatedCompletion time.Time
    
    // Alerts
    Alerts             []ExecutionAlert
}

type SegmentExecutionStatus struct {
    SegmentName        string
    Sent               int
    Planned            int
    ActualRevenue      float64
    PlannedRevenue     float64
    Status             string    // "on_track", "outperforming", "underperforming"
}

type ExecutionAlert struct {
    Severity           string    // "info", "warning", "critical"
    Message            string
    Timestamp          time.Time
}

// GetExecutionStatus returns real-time execution status
func (et *ExecutionTracker) GetExecutionStatus(
    ctx context.Context,
    planID string,
) (*ExecutionStatus, error) {
    
    // Get plan details
    plan, err := et.db.GetPlan(ctx, planID)
    if err != nil {
        return nil, err
    }
    
    approvedPeriod := plan.GetApprovedPeriod()
    
    status := &ExecutionStatus{
        PlanID:         planID,
        Period:         approvedPeriod.PeriodName,
        PlannedVolume:  approvedPeriod.TotalVolume,
        PlannedRevenue: approvedPeriod.ProjectedRevenue,
    }
    
    // Get real-time send counts
    status.SentVolume = et.db.GetPlanSendCount(ctx, planID)
    status.RemainingVolume = status.PlannedVolume - status.SentVolume
    status.PercentComplete = float64(status.SentVolume) / float64(status.PlannedVolume) * 100
    
    // Get real-time revenue
    status.ActualRevenue = et.revenueTracker.GetPlanRevenue(ctx, planID)
    status.RevenueVariance = status.ActualRevenue - status.PlannedRevenue
    if status.PlannedRevenue > 0 {
        status.PerformanceRatio = status.ActualRevenue / status.PlannedRevenue
    }
    
    // Get metrics
    metrics := et.db.GetPlanMetrics(ctx, planID)
    status.ActualOpenRate = metrics.OpenRate
    status.ActualClickRate = metrics.ClickRate
    status.ActualCVR = metrics.ConversionRate
    
    // Determine status
    if status.PercentComplete >= 100 {
        status.Status = "completed"
    } else if status.PercentComplete > 0 {
        status.Status = "executing"
    } else {
        status.Status = "pending"
    }
    
    // Check for alerts
    status.Alerts = et.checkAlerts(status, approvedPeriod)
    
    return status, nil
}

// checkAlerts identifies issues during execution
func (et *ExecutionTracker) checkAlerts(
    status *ExecutionStatus,
    plan *TimePeriodPlan,
) []ExecutionAlert {
    
    alerts := []ExecutionAlert{}
    
    // Underperforming check
    if status.PercentComplete > 20 && status.PerformanceRatio < 0.7 {
        alerts = append(alerts, ExecutionAlert{
            Severity:  "warning",
            Message:   fmt.Sprintf("Revenue tracking %.0f%% below projections", (1-status.PerformanceRatio)*100),
            Timestamp: time.Now(),
        })
    }
    
    // Outperforming check
    if status.PercentComplete > 20 && status.PerformanceRatio > 1.3 {
        alerts = append(alerts, ExecutionAlert{
            Severity:  "info",
            Message:   fmt.Sprintf("Outperforming projections by %.0f%%! 🎉", (status.PerformanceRatio-1)*100),
            Timestamp: time.Now(),
        })
    }
    
    // Behind schedule check
    expectedProgress := float64(time.Since(status.StartedAt)) / 
                        float64(status.EstimatedCompletion.Sub(status.StartedAt)) * 100
    if status.PercentComplete < expectedProgress * 0.8 {
        alerts = append(alerts, ExecutionAlert{
            Severity:  "warning",
            Message:   "Execution behind schedule - may not complete full volume",
            Timestamp: time.Now(),
        })
    }
    
    return alerts
}
```

---

## 8. Summary

### What the System Provides

| Feature | Description |
|---------|-------------|
| **Data Synthesis** | Analyzes volume capacity, audience segments, offer performance, timing patterns |
| **High-Level View** | Presents aggregated options, not per-inbox decisions |
| **Time Period Options** | Morning, First Half, Full Day plans to choose from |
| **Revenue Projections** | Predicted revenue, RPE, open/click rates per option |
| **AI Explanation** | Human-readable explanation of why the plan is structured this way |
| **Approval Workflow** | Review and approve before any sending occurs |
| **Execution Tracking** | Real-time monitoring of approved plan vs projections |

### Approval Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│                                                                     │
│  1. GENERATE  →  2. REVIEW  →  3. APPROVE  →  4. EXECUTE  →  5. TRACK │
│                                                                     │
│  AI analyzes     User sees     User selects   System sends   Real-time │
│  all metrics     3 options     a period       automatically  monitoring │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

---

**Document End**
