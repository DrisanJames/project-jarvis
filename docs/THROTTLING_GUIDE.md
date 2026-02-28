# Email Throttling Infrastructure Guide

## Overview

The IGNITE Mailing Platform implements a multi-level throttling system designed to:
- Protect sender reputation across all ESPs
- Comply with ESP rate limits
- Enable intelligent send time distribution
- Support warming of new IPs/domains

---

## Throttling Levels

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           THROTTLING HIERARCHY                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Level 1: CAMPAIGN THROTTLE (User-Configurable)                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  Presets: instant | gentle | moderate | careful | custom            │   │
│  │  Controls: Per-minute rate, duration spreading                       │   │
│  │  Applied: time.Sleep() between each send                            │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                        │
│                                     ▼                                        │
│  Level 2: SENDING PROFILE LIMITS (Admin-Configurable)                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  hourly_limit: Max sends per hour per profile                        │   │
│  │  daily_limit: Max sends per day per profile                          │   │
│  │  Applied: Pre-send check, blocks if limit exceeded                   │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                        │
│                                     ▼                                        │
│  Level 3: ESP RATE LIMITS (Enforced by Provider)                            │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  SparkPost: Varies by plan (typically 10k-100k/hour)                 │   │
│  │  AWS SES: 14/sec sandbox, varies production (request increase)       │   │
│  │  Mailgun: Varies by plan (typically 300/min free, unlimited paid)    │   │
│  │  Applied: HTTP 429 response, automatic retry with backoff            │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Throttle Presets

### Preset Definitions

```go
// ThrottlePreset defines rate limiting parameters
type ThrottlePreset struct {
    Name        string // Display name
    Description string // User-friendly description
    PerMinute   int    // Maximum sends per minute
    PerHour     int    // Maximum sends per hour (soft guidance)
}

var ThrottlePresets = map[string]ThrottlePreset{
    "instant": {
        Name:        "Instant",
        Description: "Maximum speed - use for time-sensitive transactional emails",
        PerMinute:   1000,
        PerHour:     50000,
    },
    "gentle": {
        Name:        "Gentle",
        Description: "Balanced pace - recommended for most marketing campaigns",
        PerMinute:   100,
        PerHour:     5000,
    },
    "moderate": {
        Name:        "Moderate",
        Description: "Conservative pace - good for engagement-focused campaigns",
        PerMinute:   50,
        PerHour:     2500,
    },
    "careful": {
        Name:        "Careful",
        Description: "Very slow pace - for IP/domain warming or cold lists",
        PerMinute:   20,
        PerHour:     1000,
    },
}
```

### Preset Comparison Table

| Preset | Per Minute | Per Hour | Delay | 10K Campaign | 100K Campaign | 1M Campaign |
|--------|-----------|----------|-------|--------------|---------------|-------------|
| `instant` | 1,000 | 50,000 | 60ms | 10 min | 1.7 hr | 16.7 hr |
| `gentle` | 100 | 5,000 | 600ms | 1.7 hr | 16.7 hr | 166.7 hr |
| `moderate` | 50 | 2,500 | 1.2s | 3.3 hr | 33.3 hr | 333 hr |
| `careful` | 20 | 1,000 | 3s | 8.3 hr | 83.3 hr | 833 hr |

### Delay Calculation

```go
// Calculate delay between individual sends
func calculateSendDelay(preset ThrottlePreset) time.Duration {
    if preset.PerMinute <= 0 {
        return 0 // No throttling
    }
    
    // Convert per-minute rate to milliseconds between sends
    // 60,000 ms per minute / sends per minute = ms per send
    delayMs := 60000 / preset.PerMinute
    
    return time.Duration(delayMs) * time.Millisecond
}

// Examples:
// instant:  60000 / 1000 = 60ms   (16.67 sends/second)
// gentle:   60000 / 100  = 600ms  (1.67 sends/second)
// moderate: 60000 / 50   = 1200ms (0.83 sends/second)
// careful:  60000 / 20   = 3000ms (0.33 sends/second)
```

---

## Custom Throttling

### Duration-Based Spreading

For campaigns where you want to spread sends over a specific time window:

```json
{
  "throttle_speed": "custom",
  "throttle_duration_hours": 8
}
```

**Calculation:**
```go
func calculateDurationBasedRate(totalRecipients, durationHours int) int {
    if durationHours <= 0 {
        return 1000 // Default to instant
    }
    
    totalMinutes := durationHours * 60
    
    // Calculate required rate, adding 1 to ensure completion
    rate := (totalRecipients / totalMinutes) + 1
    
    return rate
}

// Examples:
// 100,000 recipients over 8 hours:
//   totalMinutes = 8 * 60 = 480
//   rate = (100000 / 480) + 1 = 209 per minute
//   delay = 60000 / 209 = 287ms

// 50,000 recipients over 4 hours:
//   totalMinutes = 4 * 60 = 240
//   rate = (50000 / 240) + 1 = 209 per minute
//   delay = 60000 / 209 = 287ms

// 10,000 recipients over 24 hours:
//   totalMinutes = 24 * 60 = 1440
//   rate = (10000 / 1440) + 1 = 8 per minute
//   delay = 60000 / 8 = 7500ms (7.5 seconds)
```

### Custom Rate Setting

For specific per-minute rates:

```json
{
  "throttle_speed": "custom",
  "throttle_rate_per_minute": 75
}
```

---

## Sending Profile Rate Limits

### Profile Structure

```go
type SendingProfile struct {
    // ... other fields ...
    
    // Rate Limits
    HourlyLimit        int `json:"hourly_limit"`        // Max sends per hour
    DailyLimit         int `json:"daily_limit"`         // Max sends per day
    CurrentHourlyCount int `json:"current_hourly_count"` // Current hour's count
    CurrentDailyCount  int `json:"current_daily_count"`  // Current day's count
}
```

### Rate Limit Checking

```go
func (p *SendingProfile) CanSend() (bool, string) {
    now := time.Now()
    
    // Reset hourly counter if hour changed
    if now.Truncate(time.Hour) != p.lastHourReset.Truncate(time.Hour) {
        p.CurrentHourlyCount = 0
        p.lastHourReset = now
    }
    
    // Reset daily counter if day changed
    if now.Truncate(24*time.Hour) != p.lastDayReset.Truncate(24*time.Hour) {
        p.CurrentDailyCount = 0
        p.lastDayReset = now
    }
    
    // Check limits
    if p.CurrentHourlyCount >= p.HourlyLimit {
        return false, fmt.Sprintf("hourly limit reached (%d/%d)", p.CurrentHourlyCount, p.HourlyLimit)
    }
    
    if p.CurrentDailyCount >= p.DailyLimit {
        return false, fmt.Sprintf("daily limit reached (%d/%d)", p.CurrentDailyCount, p.DailyLimit)
    }
    
    return true, ""
}

func (p *SendingProfile) RecordSend() {
    p.CurrentHourlyCount++
    p.CurrentDailyCount++
}
```

### Recommended Limits by ESP

| ESP | Recommended Hourly | Recommended Daily | Notes |
|-----|-------------------|-------------------|-------|
| SparkPost | 10,000 - 50,000 | 100,000 - 500,000 | Varies by plan |
| AWS SES | 5,000 - 50,000 | 50,000 - 500,000 | Request limit increase |
| Mailgun | 8,000 - 30,000 | 80,000 - 300,000 | Varies by plan |
| SendGrid | 10,000 - 100,000 | 100,000 - 1,000,000 | Varies by plan |

---

## ESP Rate Limit Handling

### 429 Response Handling

```go
func (svc *MailingService) sendWithRetry(ctx context.Context, sendFunc func() (map[string]interface{}, error)) (map[string]interface{}, error) {
    maxRetries := 3
    baseDelay := 1 * time.Second
    
    for attempt := 0; attempt < maxRetries; attempt++ {
        result, err := sendFunc()
        
        if err == nil {
            return result, nil
        }
        
        // Check if rate limited
        if isRateLimitError(err) {
            delay := baseDelay * time.Duration(1<<attempt) // Exponential backoff
            
            select {
            case <-ctx.Done():
                return nil, ctx.Err()
            case <-time.After(delay):
                continue
            }
        }
        
        // Non-rate-limit error, don't retry
        return nil, err
    }
    
    return nil, fmt.Errorf("max retries exceeded")
}

func isRateLimitError(err error) bool {
    if httpErr, ok := err.(*HTTPError); ok {
        return httpErr.StatusCode == 429
    }
    return strings.Contains(err.Error(), "rate limit") ||
           strings.Contains(err.Error(), "too many requests")
}
```

### ESP-Specific Rate Limits

**SparkPost:**
- Free: 500/month, 100/day
- Starter: 15,000/month
- Premier: 100,000+/month
- Rate limit header: `X-RateLimit-Remaining`

**AWS SES:**
- Sandbox: 200/day, 1/second
- Production: Request increase (typical: 50,000+/day)
- Rate tracked in dashboard

**Mailgun:**
- Free: 5,000/month
- Flex: Pay per use
- Foundation+: Unlimited
- Rate limit: 300/min on free tier

---

## Multi-ESP Quota Distribution

### Quota Configuration

When using multiple ESPs for a single campaign:

```json
{
  "esp_quotas": [
    {"profile_id": "sparkpost-uuid", "percentage": 50},
    {"profile_id": "ses-uuid", "percentage": 30},
    {"profile_id": "mailgun-uuid", "percentage": 20}
  ]
}
```

### Distribution Algorithm

```go
type ESPQuota struct {
    ProfileID  string `json:"profile_id"`
    Percentage int    `json:"percentage"` // Must sum to 100
}

func selectESP(quotas []ESPQuota, subscriberIndex int) string {
    // Use subscriber index for deterministic distribution
    r := subscriberIndex % 100
    
    cumulative := 0
    for _, quota := range quotas {
        cumulative += quota.Percentage
        if r < cumulative {
            return quota.ProfileID
        }
    }
    
    // Fallback to first ESP
    return quotas[0].ProfileID
}

// For 50/30/20 split:
// Indices 0-49:   SparkPost (50%)
// Indices 50-79:  SES (30%)
// Indices 80-99:  Mailgun (20%)
```

### Weighted Round-Robin (Alternative)

```go
type ESPRouter struct {
    mu       sync.Mutex
    quotas   []ESPQuota
    counters map[string]int
}

func (r *ESPRouter) Next() string {
    r.mu.Lock()
    defer r.mu.Unlock()
    
    // Find ESP with lowest relative usage
    var selected string
    var lowestRatio float64 = math.MaxFloat64
    
    for _, quota := range r.quotas {
        ratio := float64(r.counters[quota.ProfileID]) / float64(quota.Percentage)
        if ratio < lowestRatio {
            lowestRatio = ratio
            selected = quota.ProfileID
        }
    }
    
    r.counters[selected]++
    return selected
}
```

---

## IP/Domain Warming

### Warming Schedule

For new IPs or domains, follow a gradual warming schedule:

| Week | Daily Volume | Throttle Preset | Notes |
|------|-------------|-----------------|-------|
| 1 | 50 - 100 | careful | Start slow |
| 2 | 200 - 500 | careful | Gradually increase |
| 3 | 1,000 - 2,000 | moderate | Monitor engagement |
| 4 | 5,000 - 10,000 | moderate | Check reputation |
| 5 | 20,000 - 50,000 | gentle | Near full volume |
| 6+ | Full volume | gentle/instant | Maintain reputation |

### Warming Best Practices

1. **Start with engaged subscribers**
   - Mail users who have opened/clicked recently
   - Avoid cold/inactive lists during warming

2. **Monitor closely**
   - Check bounce rates daily (target: < 2%)
   - Check complaint rates daily (target: < 0.1%)
   - Watch for blacklist entries

3. **Respond to issues immediately**
   - If bounce rate spikes, reduce volume
   - If complaints spike, pause and investigate
   - If blacklisted, stop and resolve

4. **Segment by ISP**
   - Gmail tends to be more forgiving
   - Microsoft (Outlook/Hotmail) is stricter
   - Yahoo requires consistent volume

### Warming Implementation

```go
type WarmingSchedule struct {
    DayNumber     int `json:"day_number"`
    MaxDaily      int `json:"max_daily"`
    ThrottleSpeed string `json:"throttle_speed"`
}

var DefaultWarmingSchedule = []WarmingSchedule{
    {DayNumber: 1, MaxDaily: 50, ThrottleSpeed: "careful"},
    {DayNumber: 2, MaxDaily: 100, ThrottleSpeed: "careful"},
    {DayNumber: 3, MaxDaily: 200, ThrottleSpeed: "careful"},
    {DayNumber: 4, MaxDaily: 400, ThrottleSpeed: "careful"},
    {DayNumber: 5, MaxDaily: 800, ThrottleSpeed: "careful"},
    {DayNumber: 6, MaxDaily: 1500, ThrottleSpeed: "careful"},
    {DayNumber: 7, MaxDaily: 3000, ThrottleSpeed: "moderate"},
    {DayNumber: 14, MaxDaily: 10000, ThrottleSpeed: "moderate"},
    {DayNumber: 21, MaxDaily: 30000, ThrottleSpeed: "gentle"},
    {DayNumber: 28, MaxDaily: 100000, ThrottleSpeed: "gentle"},
}

func GetWarmingLimits(profileCreatedAt time.Time) (int, string) {
    daysSinceCreation := int(time.Since(profileCreatedAt).Hours() / 24)
    
    var maxDaily int
    var throttle string
    
    for _, stage := range DefaultWarmingSchedule {
        if daysSinceCreation >= stage.DayNumber {
            maxDaily = stage.MaxDaily
            throttle = stage.ThrottleSpeed
        }
    }
    
    return maxDaily, throttle
}
```

---

## Time-of-Day Optimization

### Send Time Windows

Optimal send times vary by audience, but general guidelines:

| Audience | Best Days | Best Times | Worst Times |
|----------|-----------|------------|-------------|
| B2B | Tue-Thu | 10am-11am, 2pm-3pm | Weekends, early morning |
| B2C | Tue-Thu | 8am-10am, 7pm-9pm | Late night |
| E-commerce | Thu-Sat | 10am, 8pm | Mon-Tue early |
| Newsletter | Tue-Wed | 6am-8am, 5pm-7pm | Weekends |

### Time Zone Handling

```go
func scheduleForLocalTime(targetTime time.Time, timezone string) time.Time {
    loc, err := time.LoadLocation(timezone)
    if err != nil {
        loc = time.UTC
    }
    
    // Convert target time to the specified timezone
    return time.Date(
        targetTime.Year(),
        targetTime.Month(),
        targetTime.Day(),
        targetTime.Hour(),
        targetTime.Minute(),
        0, 0, loc,
    )
}

// Example: Schedule for 10am in each subscriber's timezone
func scheduleBySubscriberTimezone(subscribers []Subscriber, baseTime time.Time) []ScheduledSend {
    var scheduled []ScheduledSend
    
    for _, sub := range subscribers {
        sendTime := scheduleForLocalTime(baseTime, sub.Timezone)
        scheduled = append(scheduled, ScheduledSend{
            SubscriberID: sub.ID,
            SendAt:       sendTime,
        })
    }
    
    // Sort by send time for efficient processing
    sort.Slice(scheduled, func(i, j int) bool {
        return scheduled[i].SendAt.Before(scheduled[j].SendAt)
    })
    
    return scheduled
}
```

---

## Monitoring & Metrics

### Key Throttling Metrics

```go
type ThrottleMetrics struct {
    CampaignID       string        `json:"campaign_id"`
    PresetUsed       string        `json:"preset_used"`
    TargetRate       int           `json:"target_rate_per_minute"`
    ActualRate       float64       `json:"actual_rate_per_minute"`
    TotalSent        int           `json:"total_sent"`
    TotalDuration    time.Duration `json:"total_duration"`
    RateLimitHits    int           `json:"rate_limit_hits"`
    ProfileLimitHits int           `json:"profile_limit_hits"`
}

func (m *ThrottleMetrics) ActualRatePerMinute() float64 {
    if m.TotalDuration == 0 {
        return 0
    }
    minutes := m.TotalDuration.Minutes()
    if minutes == 0 {
        return float64(m.TotalSent)
    }
    return float64(m.TotalSent) / minutes
}
```

### Dashboard Metrics

```json
GET /api/mailing/throttle/status

{
  "current_campaigns": [
    {
      "campaign_id": "uuid",
      "name": "February Newsletter",
      "throttle_speed": "gentle",
      "target_rate": 100,
      "current_rate": 98.5,
      "sent": 4500,
      "remaining": 5500,
      "estimated_completion": "2026-02-03T15:30:00Z"
    }
  ],
  "profile_usage": [
    {
      "profile_id": "uuid",
      "name": "SparkPost - Primary",
      "hourly_limit": 10000,
      "hourly_used": 4500,
      "daily_limit": 100000,
      "daily_used": 25000,
      "reset_at": "2026-02-03T14:00:00Z"
    }
  ],
  "rate_limit_events": {
    "last_hour": 0,
    "last_24h": 2
  }
}
```

---

## Troubleshooting

### Common Issues

**Issue: Sends slower than expected**
- Check: Profile rate limits
- Check: ESP rate limit responses (429s)
- Check: Network latency to ESP
- Check: Database connection pool

**Issue: Rate limit errors from ESP**
- Solution: Reduce campaign throttle
- Solution: Increase profile limits (if ESP allows)
- Solution: Split across multiple ESP profiles

**Issue: Campaign taking too long**
- Check: Throttle preset appropriateness
- Consider: Duration-based spreading
- Consider: Multi-ESP distribution

### Debug Logging

```go
func (svc *MailingService) sendWithLogging(ctx context.Context, sub Subscriber, campaign Campaign) {
    start := time.Now()
    
    result, err := svc.send(ctx, sub, campaign)
    
    duration := time.Since(start)
    
    log.Printf("SEND: campaign=%s subscriber=%s duration=%v success=%v error=%v",
        campaign.ID, sub.ID, duration, err == nil, err)
    
    // Track metrics
    svc.metrics.RecordSend(campaign.ID, duration, err == nil)
}
```

---

## Quick Reference

### API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/mailing/throttle/status` | GET | Current throttle status |
| `/api/mailing/throttle/presets` | GET | Available presets |
| `/api/mailing/sending-profiles/{id}` | GET | Profile limits |
| `/api/mailing/campaigns/{id}` | GET | Campaign throttle config |

### Throttle Preset Quick Reference

```
instant  → 1000/min  → 60ms delay   → Use for: Alerts, transactional
gentle   → 100/min   → 600ms delay  → Use for: Newsletters
moderate → 50/min    → 1200ms delay → Use for: Marketing
careful  → 20/min    → 3000ms delay → Use for: Warming, cold lists
```

### Formula Reference

```
delay_ms = 60000 / sends_per_minute

sends_per_minute = total_recipients / (duration_hours * 60)

estimated_hours = total_recipients / (sends_per_minute * 60)
```

---

*Last Updated: February 2026*
*Version: 1.0*
