# Email Sending Infrastructure Documentation

## Table of Contents
1. [Architecture Overview](#architecture-overview)
2. [ESP Integrations](#esp-integrations)
3. [Sending Profiles](#sending-profiles)
4. [Throttling System](#throttling-system)
5. [Campaign Sending Flow](#campaign-sending-flow)
6. [Tracking System](#tracking-system)
7. [Suppression Checking](#suppression-checking)
8. [Webhooks & Event Processing](#webhooks--event-processing)
9. [API Reference](#api-reference)
10. [Configuration Guide](#configuration-guide)

---

## Architecture Overview

The IGNITE Mailing Platform provides an enterprise-grade email sending infrastructure supporting multiple Email Service Providers (ESPs) with intelligent throttling, real-time tracking, and comprehensive suppression management.

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Campaign Creation                                │
│                    (name, subject, content, list)                        │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                         Sending Profile Selection                        │
│              (SparkPost, SES, Mailgun, SendGrid, SMTP)                  │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                         Subscriber Retrieval                             │
│                    (List-based or Segment-based)                         │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌───────────────────────────────────┬─────────────────────────────────────┐
│     Per-Subscriber Processing     │                                     │
│                                   │                                     │
│  ┌─────────────────────────┐     │     ┌─────────────────────────┐     │
│  │  1. Suppression Check   │─────┼────▶│  Skip if Suppressed     │     │
│  └─────────────────────────┘     │     └─────────────────────────┘     │
│              │                   │                                     │
│              ▼                   │                                     │
│  ┌─────────────────────────┐     │                                     │
│  │  2. Tracking Injection  │     │                                     │
│  │  (Open pixel + Links)   │     │                                     │
│  └─────────────────────────┘     │                                     │
│              │                   │                                     │
│              ▼                   │                                     │
│  ┌─────────────────────────┐     │                                     │
│  │  3. ESP API Call        │     │                                     │
│  └─────────────────────────┘     │                                     │
│              │                   │                                     │
│              ▼                   │                                     │
│  ┌─────────────────────────┐     │                                     │
│  │  4. Record Sent Event   │     │                                     │
│  └─────────────────────────┘     │                                     │
│              │                   │                                     │
│              ▼                   │                                     │
│  ┌─────────────────────────┐     │                                     │
│  │  5. Apply Throttle Delay│     │                                     │
│  └─────────────────────────┘     │                                     │
└───────────────────────────────────┴─────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                         Campaign Completion                              │
│               (Update stats, status, profile usage)                      │
└─────────────────────────────────────────────────────────────────────────┘
```

### Key Components

| Component | File | Purpose |
|-----------|------|---------|
| Campaign Builder | `internal/api/campaign_builder.go` | Campaign CRUD and send orchestration |
| ESP Handlers | `internal/api/mailing_handlers_full.go` | ESP-specific send implementations |
| Sending Profiles | `internal/api/sending_profiles_handlers.go` | ESP credential management |
| Tracking Service | `internal/mailing/tracking.go` | Open/click tracking injection |
| Suppression Engine | `internal/suppression/engine.go` | Fast suppression matching |
| Email Sender | `internal/mailing/email_sender.go` | Low-level throttling |

---

## ESP Integrations

### Supported Email Service Providers

| ESP | Implementation | Auth Method | Status |
|-----|---------------|-------------|--------|
| SparkPost | REST API | API Key | ✅ Production |
| AWS SES | AWS CLI/SDK | IAM Credentials | ✅ Production |
| Mailgun | REST API | API Key + Domain | ✅ Production |
| SendGrid | REST API | API Key | ✅ Production |
| SMTP | Direct Connection | Username/Password | 🔧 Basic |

### SparkPost Integration

**Endpoint:** `https://api.sparkpost.com/api/v1/transmissions`

**Function Signature:**
```go
func sendViaSparkPost(ctx context.Context, to, fromEmail, fromName, subject, htmlContent, textContent string) (map[string]interface{}, error)
```

**Request Payload:**
```json
{
  "recipients": [{"address": {"email": "recipient@example.com"}}],
  "content": {
    "from": {"email": "sender@domain.com", "name": "Sender Name"},
    "subject": "Email Subject",
    "html": "<html>...</html>",
    "text": "Plain text..."
  },
  "options": {
    "open_tracking": true,
    "click_tracking": true
  },
  "metadata": {
    "campaign_id": "uuid",
    "subscriber_id": "uuid"
  }
}
```

**Features:**
- Native open/click tracking
- Metadata passthrough for webhook correlation
- Automatic bounce classification
- Engagement scoring

### AWS SES Integration

**Implementation:** AWS CLI with default profile

**Function Signature:**
```go
func sendViaSES(ctx context.Context, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error)
```

**CLI Command:**
```bash
aws ses send-email \
  --from "Name <sender@domain.com>" \
  --to "recipient@example.com" \
  --subject "Subject Line" \
  --html "HTML Content" \
  --text "Text Content" \
  --reply-to-addresses "reply@domain.com" \
  --region us-east-1
```

**Features:**
- Uses default AWS credentials
- SNS notifications for bounces/complaints
- Virtual Deliverability Manager (VDM) integration
- Reputation dashboard

### Mailgun Integration

**Endpoint:** `https://api.mailgun.net/v3/{domain}/messages`

**Function Signature:**
```go
func sendViaMailgun(ctx context.Context, apiKey, domain, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error)
```

**Request (Form-Encoded):**
```
from=Name <sender@domain.com>
to=recipient@example.com
subject=Email Subject
html=<html>...</html>
text=Plain text...
h:Reply-To=reply@domain.com
o:tracking=yes
o:tracking-clicks=htmlonly
o:tracking-opens=yes
```

**Features:**
- Domain-based sending
- Webhooks for all event types
- Built-in click/open tracking
- Email validation API

### SendGrid Integration

**Endpoint:** `https://api.sendgrid.com/v3/mail/send`

**Function Signature:**
```go
func sendViaSendGrid(ctx context.Context, apiKey, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error)
```

**Request Payload:**
```json
{
  "personalizations": [{"to": [{"email": "recipient@example.com"}]}],
  "from": {"email": "sender@domain.com", "name": "Sender Name"},
  "subject": "Email Subject",
  "content": [
    {"type": "text/plain", "value": "Plain text..."},
    {"type": "text/html", "value": "<html>...</html>"}
  ],
  "reply_to": {"email": "reply@domain.com"}
}
```

**Features:**
- Activity feed integration
- Template support
- Dynamic content
- Advanced analytics

---

## Sending Profiles

Sending profiles encapsulate ESP credentials, domain configuration, and rate limits.

### Profile Structure

```go
type SendingProfile struct {
    ID                string    `json:"id"`
    OrganizationID    string    `json:"organization_id"`
    Name              string    `json:"name"`
    Description       string    `json:"description"`
    VendorType        string    `json:"vendor_type"`  // sparkpost, ses, mailgun, sendgrid, smtp
    
    // Credentials
    APIKey            string    `json:"api_key,omitempty"`
    SMTPHost          string    `json:"smtp_host,omitempty"`
    SMTPPort          int       `json:"smtp_port,omitempty"`
    SMTPUsername      string    `json:"smtp_username,omitempty"`
    SMTPPassword      string    `json:"smtp_password,omitempty"`
    SMTPEncryption    string    `json:"smtp_encryption,omitempty"` // none, ssl, tls
    
    // Sender Identity
    FromName          string    `json:"from_name"`
    FromEmail         string    `json:"from_email"`
    ReplyEmail        string    `json:"reply_email"`
    
    // Domain Configuration
    SendingDomain     string    `json:"sending_domain"`
    TrackingDomain    string    `json:"tracking_domain,omitempty"`
    
    // Verification Status
    SPFVerified       bool      `json:"spf_verified"`
    DKIMVerified      bool      `json:"dkim_verified"`
    DMARCVerified     bool      `json:"dmarc_verified"`
    DomainVerified    bool      `json:"domain_verified"`
    CredentialsVerified bool    `json:"credentials_verified"`
    
    // Rate Limits
    HourlyLimit       int       `json:"hourly_limit"`
    DailyLimit        int       `json:"daily_limit"`
    CurrentHourlyCount int      `json:"current_hourly_count"`
    CurrentDailyCount int       `json:"current_daily_count"`
    
    // Status
    Status            string    `json:"status"` // active, inactive, suspended
    IsDefault         bool      `json:"is_default"`
}
```

### API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/mailing/sending-profiles` | List all profiles |
| POST | `/api/mailing/sending-profiles` | Create new profile |
| GET | `/api/mailing/sending-profiles/{id}` | Get profile details |
| PUT | `/api/mailing/sending-profiles/{id}` | Update profile |
| DELETE | `/api/mailing/sending-profiles/{id}` | Delete profile |
| POST | `/api/mailing/sending-profiles/{id}/verify` | Verify credentials |
| POST | `/api/mailing/sending-profiles/{id}/test` | Send test email |
| GET | `/api/mailing/sending-profiles/vendors` | List supported vendors |

### Example: Creating a SparkPost Profile

```bash
curl -X POST http://localhost:8080/api/mailing/sending-profiles \
  -H "Content-Type: application/json" \
  -d '{
    "name": "SparkPost - Primary",
    "description": "Main SparkPost account for newsletters",
    "vendor_type": "sparkpost",
    "api_key": "your-sparkpost-api-key",
    "from_name": "IGNITE News",
    "from_email": "news@newsletter.yourdomain.com",
    "reply_email": "support@yourdomain.com",
    "sending_domain": "newsletter.yourdomain.com",
    "tracking_domain": "track.yourdomain.com",
    "hourly_limit": 10000,
    "daily_limit": 100000,
    "is_default": true
  }'
```

---

## Throttling System

The throttling system controls email send rate to protect sender reputation and comply with ESP rate limits.

### Throttle Presets

| Preset | Per Minute | Per Hour | Delay Between Sends | Use Case |
|--------|-----------|----------|---------------------|----------|
| `instant` | 1,000 | 50,000 | 60ms | Time-sensitive alerts |
| `gentle` | 100 | 5,000 | 600ms | Standard newsletters |
| `moderate` | 50 | 2,500 | 1.2s | Engagement campaigns |
| `careful` | 20 | 1,000 | 3s | New domains/IPs |
| `custom` | User-defined | User-defined | Calculated | Special requirements |

### Preset Definitions

```go
var ThrottlePresets = map[string]ThrottlePreset{
    "instant": {
        Name:        "Instant",
        Description: "Maximum speed - use for time-sensitive emails",
        PerMinute:   1000,
        PerHour:     50000,
    },
    "gentle": {
        Name:        "Gentle",
        Description: "Balanced pace - recommended for most campaigns",
        PerMinute:   100,
        PerHour:     5000,
    },
    "moderate": {
        Name:        "Moderate",
        Description: "Conservative pace - good for engagement campaigns",
        PerMinute:   50,
        PerHour:     2500,
    },
    "careful": {
        Name:        "Careful",
        Description: "Very slow pace - for warming up new IPs/domains",
        PerMinute:   20,
        PerHour:     1000,
    },
}
```

### Throttle Calculation

```go
// Calculate delay between sends
func calculateThrottleDelay(preset ThrottlePreset) time.Duration {
    if preset.PerMinute <= 0 {
        return 0 // No delay for instant
    }
    return time.Duration(60000/preset.PerMinute) * time.Millisecond
}

// Example calculations:
// instant:  60000 / 1000 = 60ms delay
// gentle:   60000 / 100  = 600ms delay
// moderate: 60000 / 50   = 1200ms delay
// careful:  60000 / 20   = 3000ms delay
```

### Custom Throttling

For campaigns with specific requirements, use custom throttling:

```json
{
  "throttle_speed": "custom",
  "throttle_rate_per_minute": 75,
  "throttle_duration_hours": 8
}
```

**Duration-Based Spreading:**

When `throttle_duration_hours` is set, the system calculates the rate needed to spread sends over the duration:

```go
func calculateDurationBasedRate(totalRecipients int, durationHours int) int {
    totalMinutes := durationHours * 60
    return (totalRecipients / totalMinutes) + 1 // +1 to ensure completion
}

// Example: 100,000 recipients over 8 hours
// totalMinutes = 8 * 60 = 480
// rate = (100000 / 480) + 1 = 209 per minute
```

### Multi-Level Throttling

The system implements throttling at multiple levels:

```
┌─────────────────────────────────────────────────────────────┐
│                  Level 1: Campaign Throttle                  │
│           (throttle_speed preset or custom rate)             │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                  Level 2: Profile Rate Limits                │
│           (hourly_limit, daily_limit per profile)            │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                  Level 3: ESP Rate Limits                    │
│           (enforced by ESP API, returns 429 if exceeded)     │
└─────────────────────────────────────────────────────────────┘
```

### Rate Counter Implementation

```go
type RateCounter struct {
    mu          sync.Mutex
    hourlyCount int
    minuteCount int
    lastHour    time.Time
    lastMinute  time.Time
}

func (rc *RateCounter) CanSend(hourlyLimit, minuteLimit int) bool {
    rc.mu.Lock()
    defer rc.mu.Unlock()
    
    now := time.Now()
    
    // Reset counters on boundary
    if now.Truncate(time.Hour) != rc.lastHour.Truncate(time.Hour) {
        rc.hourlyCount = 0
        rc.lastHour = now
    }
    if now.Truncate(time.Minute) != rc.lastMinute.Truncate(time.Minute) {
        rc.minuteCount = 0
        rc.lastMinute = now
    }
    
    // Check limits
    if rc.hourlyCount >= hourlyLimit || rc.minuteCount >= minuteLimit {
        return false
    }
    
    rc.hourlyCount++
    rc.minuteCount++
    return true
}
```

---

## Campaign Sending Flow

### Complete Flow Diagram

```
User clicks "Send Campaign"
            │
            ▼
┌───────────────────────────────────────┐
│  1. HandleSendCampaign                │
│     - Validate campaign exists        │
│     - Check status (draft/scheduled)  │
│     - Load sending profile            │
└───────────────────────────────────────┘
            │
            ▼
┌───────────────────────────────────────┐
│  2. Update Campaign Status            │
│     - status = 'sending'              │
│     - started_at = NOW()              │
└───────────────────────────────────────┘
            │
            ▼
┌───────────────────────────────────────┐
│  3. Get Subscribers                   │
│     - Query by list_id or segment_id  │
│     - Filter status = 'confirmed'     │
│     - Apply max_recipients limit      │
└───────────────────────────────────────┘
            │
            ▼
┌───────────────────────────────────────┐
│  4. Calculate Throttle Settings       │
│     - Load preset by throttle_speed   │
│     - Calculate send delay            │
└───────────────────────────────────────┘
            │
            ▼
┌───────────────────────────────────────┐
│  5. FOR EACH Subscriber:              │
│                                       │
│  ┌─────────────────────────────────┐ │
│  │ a. Check Suppression            │ │
│  │    - Query mailing_suppressions │ │
│  │    - Skip if suppressed         │ │
│  └─────────────────────────────────┘ │
│              │                        │
│              ▼                        │
│  ┌─────────────────────────────────┐ │
│  │ b. Generate Email ID (UUID)     │ │
│  └─────────────────────────────────┘ │
│              │                        │
│              ▼                        │
│  ┌─────────────────────────────────┐ │
│  │ c. Inject Tracking              │ │
│  │    - Open pixel before </body>  │ │
│  │    - Replace hrefs with tracked │ │
│  └─────────────────────────────────┘ │
│              │                        │
│              ▼                        │
│  ┌─────────────────────────────────┐ │
│  │ d. Select ESP by vendor_type    │ │
│  │    - sparkpost → sendViaSparkPost│
│  │    - ses → sendViaSES           │ │
│  │    - mailgun → sendViaMailgun   │ │
│  │    - sendgrid → sendViaSendGrid │ │
│  └─────────────────────────────────┘ │
│              │                        │
│              ▼                        │
│  ┌─────────────────────────────────┐ │
│  │ e. Record Sent Event            │ │
│  │    - Insert mailing_tracking_   │ │
│  │      events (type='sent')       │ │
│  └─────────────────────────────────┘ │
│              │                        │
│              ▼                        │
│  ┌─────────────────────────────────┐ │
│  │ f. Apply Throttle Delay         │ │
│  │    - time.Sleep(sendDelay)      │ │
│  └─────────────────────────────────┘ │
│                                       │
└───────────────────────────────────────┘
            │
            ▼
┌───────────────────────────────────────┐
│  6. Campaign Completion               │
│     - Calculate final status:         │
│       • 'completed' (no failures)     │
│       • 'completed_with_errors'       │
│       • 'failed' (all failed)         │
│     - Update sent_count               │
│     - Set completed_at = NOW()        │
│     - Update profile usage stats      │
└───────────────────────────────────────┘
            │
            ▼
┌───────────────────────────────────────┐
│  7. Return Response                   │
│     {                                 │
│       "campaign_id": "uuid",          │
│       "status": "completed",          │
│       "sent": 1000,                   │
│       "failed": 5,                    │
│       "suppressed": 50,               │
│       "total_targeted": 1055          │
│     }                                 │
└───────────────────────────────────────┘
```

### Code Implementation

```go
func (cb *CampaignBuilder) HandleSendCampaign(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    id := chi.URLParam(r, "id")
    
    // 1. Load campaign
    var campaign struct {
        Subject       string
        FromName      string
        FromEmail     string
        HTMLContent   string
        TextContent   string
        ListID        sql.NullString
        SegmentID     sql.NullString
        ProfileID     sql.NullString
        ThrottleSpeed string
        MaxRecipients sql.NullInt64
        Status        string
    }
    
    err := cb.db.QueryRowContext(ctx, `
        SELECT subject, from_name, from_email, 
               COALESCE(html_content, ''), COALESCE(plain_content, ''),
               list_id, segment_id, sending_profile_id,
               COALESCE(throttle_speed, 'gentle'), max_recipients, status
        FROM mailing_campaigns WHERE id = $1
    `, id).Scan(...)
    
    // 2. Validate status
    if campaign.Status != "draft" && campaign.Status != "scheduled" {
        http.Error(w, "cannot send campaign in this status", http.StatusBadRequest)
        return
    }
    
    // 3. Load sending profile
    var profile struct {
        ID         string
        VendorType string
        APIKey     sql.NullString
    }
    cb.db.QueryRowContext(ctx, `
        SELECT id, vendor_type, api_key 
        FROM mailing_sending_profiles WHERE id = $1
    `, campaign.ProfileID.String).Scan(&profile.ID, &profile.VendorType, &profile.APIKey)
    
    // 4. Update status to sending
    cb.db.ExecContext(ctx, `
        UPDATE mailing_campaigns 
        SET status = 'sending', started_at = NOW(), updated_at = NOW() 
        WHERE id = $1
    `, id)
    
    // 5. Get subscribers
    subscribers := cb.getSubscribers(ctx, listID, segmentID, campaign.MaxRecipients)
    
    // 6. Calculate throttle
    throttle := ThrottlePresets[campaign.ThrottleSpeed]
    sendDelay := time.Duration(60000/throttle.PerMinute) * time.Millisecond
    
    // 7. Send loop
    var sent, failed, suppressed int
    for _, sub := range subscribers {
        // Check suppression
        var isSuppressed bool
        cb.db.QueryRowContext(ctx, `
            SELECT EXISTS(
                SELECT 1 FROM mailing_suppressions 
                WHERE LOWER(email) = LOWER($1) AND active = true
            )
        `, sub.Email).Scan(&isSuppressed)
        
        if isSuppressed {
            suppressed++
            continue
        }
        
        // Inject tracking
        emailID := uuid.New()
        trackedHTML := cb.mailingSvc.injectTracking(
            campaign.HTMLContent, orgID, campUUID, sub.ID, emailID)
        
        // Send via appropriate ESP
        var result map[string]interface{}
        var sendErr error
        
        switch profile.VendorType {
        case "ses":
            result, sendErr = cb.mailingSvc.sendViaSES(ctx, ...)
        case "mailgun":
            result, sendErr = cb.mailingSvc.sendViaMailgun(ctx, ...)
        case "sparkpost":
            result, sendErr = cb.mailingSvc.sendViaSparkPost(ctx, ...)
        }
        
        if sendErr == nil && result["success"] == true {
            sent++
            // Record sent event
            cb.db.ExecContext(ctx, `
                INSERT INTO mailing_tracking_events 
                (id, campaign_id, subscriber_id, email, event_type, event_time, metadata)
                VALUES ($1, $2, $3, $4, 'sent', NOW(), $5)
            `, emailID, campUUID, sub.ID, sub.Email, metadata)
        } else {
            failed++
        }
        
        // Apply throttle delay
        time.Sleep(sendDelay)
    }
    
    // 8. Update completion status
    finalStatus := "completed"
    if failed > 0 && sent == 0 {
        finalStatus = "failed"
    } else if failed > 0 {
        finalStatus = "completed_with_errors"
    }
    
    cb.db.ExecContext(ctx, `
        UPDATE mailing_campaigns 
        SET status = $1, sent_count = $2, completed_at = NOW()
        WHERE id = $3
    `, finalStatus, sent, id)
    
    // Return response
    json.NewEncoder(w).Encode(map[string]interface{}{
        "campaign_id":    id,
        "status":         finalStatus,
        "sent":           sent,
        "failed":         failed,
        "suppressed":     suppressed,
        "total_targeted": len(subscribers),
    })
}
```

---

## Tracking System

### Open Tracking

Open tracking works by injecting an invisible 1x1 pixel image that loads when the email is opened.

**Pixel Injection:**
```go
func injectOpenTracking(html string, trackingPixelURL string) string {
    pixel := fmt.Sprintf(
        `<img src="%s" width="1" height="1" style="display:none" alt="">`,
        trackingPixelURL,
    )
    
    // Inject before </body>
    if strings.Contains(html, "</body>") {
        return strings.Replace(html, "</body>", pixel+"</body>", 1)
    }
    return html + pixel
}
```

**Tracking URL Format:**
```
https://track.yourdomain.com/track/open/{base64_encoded_data}/{signature}
```

**Encoded Data:**
```json
{
  "org_id": "uuid",
  "campaign_id": "uuid",
  "subscriber_id": "uuid",
  "email_id": "uuid"
}
```

### Click Tracking

Click tracking replaces all links with tracked redirect URLs.

**Link Replacement:**
```go
func injectClickTracking(html string, baseTrackingURL string, data TrackingData) string {
    // Find all href attributes
    re := regexp.MustCompile(`href=["']([^"']+)["']`)
    
    return re.ReplaceAllStringFunc(html, func(match string) string {
        originalURL := extractURL(match)
        trackedURL := generateClickURL(baseTrackingURL, data, originalURL)
        return fmt.Sprintf(`href="%s"`, trackedURL)
    })
}
```

**Click URL Format:**
```
https://track.yourdomain.com/track/click/{base64_encoded_data}/{signature}?url={encoded_original_url}
```

### Tracking Event Handlers

**Open Tracking Handler:**
```go
func (svc *MailingService) HandleTrackOpen(w http.ResponseWriter, r *http.Request) {
    data := chi.URLParam(r, "data")
    
    // Decode tracking data
    trackingData := decodeTrackingData(data)
    
    // Record open event
    svc.db.Exec(`
        INSERT INTO mailing_tracking_events 
        (id, campaign_id, subscriber_id, email_id, event_type, event_time, ip_address, user_agent)
        VALUES ($1, $2, $3, $4, 'opened', NOW(), $5, $6)
        ON CONFLICT DO NOTHING
    `, uuid.New(), trackingData.CampaignID, trackingData.SubscriberID, 
       trackingData.EmailID, r.RemoteAddr, r.UserAgent())
    
    // Update campaign open count
    svc.db.Exec(`
        UPDATE mailing_campaigns SET open_count = open_count + 1 WHERE id = $1
    `, trackingData.CampaignID)
    
    // Return 1x1 transparent GIF
    w.Header().Set("Content-Type", "image/gif")
    w.Write(transparentGIF)
}
```

**Click Tracking Handler:**
```go
func (svc *MailingService) HandleTrackClick(w http.ResponseWriter, r *http.Request) {
    data := chi.URLParam(r, "data")
    originalURL := r.URL.Query().Get("url")
    
    // Decode tracking data
    trackingData := decodeTrackingData(data)
    
    // Record click event
    svc.db.Exec(`
        INSERT INTO mailing_tracking_events 
        (id, campaign_id, subscriber_id, email_id, event_type, event_time, 
         ip_address, user_agent, link_url)
        VALUES ($1, $2, $3, $4, 'clicked', NOW(), $5, $6, $7)
    `, uuid.New(), trackingData.CampaignID, trackingData.SubscriberID,
       trackingData.EmailID, r.RemoteAddr, r.UserAgent(), originalURL)
    
    // Update campaign click count
    svc.db.Exec(`
        UPDATE mailing_campaigns SET click_count = click_count + 1 WHERE id = $1
    `, trackingData.CampaignID)
    
    // Redirect to original URL
    http.Redirect(w, r, originalURL, http.StatusFound)
}
```

### Tracking Events Table Schema

```sql
CREATE TABLE mailing_tracking_events (
    id UUID PRIMARY KEY,
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id),
    subscriber_id UUID REFERENCES mailing_subscribers(id),
    email_id UUID,
    email VARCHAR(255),
    event_type VARCHAR(50) NOT NULL, -- sent, opened, clicked, bounced, complained, unsubscribed
    event_time TIMESTAMPTZ DEFAULT NOW(),
    ip_address VARCHAR(45),
    user_agent TEXT,
    device_type VARCHAR(50),
    link_url TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_tracking_events_campaign ON mailing_tracking_events(campaign_id);
CREATE INDEX idx_tracking_events_subscriber ON mailing_tracking_events(subscriber_id);
CREATE INDEX idx_tracking_events_type ON mailing_tracking_events(event_type);
CREATE INDEX idx_tracking_events_time ON mailing_tracking_events(event_time);
```

---

## Suppression Checking

### Suppression Engine Architecture

The suppression engine uses a two-layer approach for memory efficiency and speed:

```
┌─────────────────────────────────────────────────────────────┐
│                    Layer 1: Bloom Filter                     │
│              (O(1) probabilistic membership test)            │
│                                                              │
│  • 99%+ true negative rate                                   │
│  • ~150MB for 100M entries                                   │
│  • Instant rejection of non-suppressed emails                │
└─────────────────────────────────────────────────────────────┘
                              │
                    (if positive)
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                Layer 2: Sorted Binary Search                 │
│              (O(log n) deterministic verification)           │
│                                                              │
│  • MD5 hashes stored as sorted [16]byte array               │
│  • ~28MB for 1M entries (vs ~1.2GB for map[string]bool)     │
│  • Only checked for Bloom filter positives (~1% of queries) │
└─────────────────────────────────────────────────────────────┘
```

### Memory Comparison

| Approach | 1M Entries | 10M Entries | 100M Entries |
|----------|-----------|-------------|--------------|
| `map[string]bool` | ~1.2 GB | ~12 GB | ~120 GB |
| Sorted Slice (MD5) | ~28 MB | ~280 MB | ~2.8 GB |
| Bloom + Sorted | ~30 MB | ~300 MB | ~3 GB |

### Implementation

```go
type SuppressionMatcher struct {
    mu          sync.RWMutex
    filter      *bloom.BloomFilter
    sortedMD5s  [][16]byte
    listID      string
    loadedAt    time.Time
}

// IsSuppressed checks if an email is in the suppression list
func (m *SuppressionMatcher) IsSuppressed(email string) bool {
    // Normalize email
    email = strings.ToLower(strings.TrimSpace(email))
    
    // Compute MD5 hash
    hash := md5.Sum([]byte(email))
    
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    // Layer 1: Bloom filter check (O(1))
    if !m.filter.Test(hash[:]) {
        return false // Definitely not suppressed
    }
    
    // Layer 2: Binary search verification (O(log n))
    idx := sort.Search(len(m.sortedMD5s), func(i int) bool {
        return bytes.Compare(m.sortedMD5s[i][:], hash[:]) >= 0
    })
    
    return idx < len(m.sortedMD5s) && m.sortedMD5s[idx] == hash
}
```

### Suppression Manager (Singleton Pattern)

```go
type SuppressionManager struct {
    mu           sync.RWMutex
    loadedLists  map[string]*SuppressionMatcher
    loading      map[string]*sync.WaitGroup // Thundering herd prevention
}

var globalManager = &SuppressionManager{
    loadedLists: make(map[string]*SuppressionMatcher),
    loading:     make(map[string]*sync.WaitGroup),
}

// GetMatcher returns a matcher for the given list, loading if necessary
func (m *SuppressionManager) GetMatcher(listID string) (*SuppressionMatcher, error) {
    // Check if already loaded
    m.mu.RLock()
    if matcher, exists := m.loadedLists[listID]; exists {
        m.mu.RUnlock()
        return matcher, nil
    }
    
    // Check if currently loading (thundering herd prevention)
    if wg, loading := m.loading[listID]; loading {
        m.mu.RUnlock()
        wg.Wait()
        return m.GetMatcher(listID) // Retry after load complete
    }
    m.mu.RUnlock()
    
    // Load the list
    return m.loadList(listID)
}
```

### Suppression Categories (Global Suppression List)

| Category | Description | Auto-Add Trigger |
|----------|-------------|------------------|
| `hard_bounce` | Permanent delivery failure | ESP bounce webhook |
| `soft_bounce_promoted` | Exceeded retry threshold | 3+ soft bounces |
| `spam_complaint` | FBL/ISP complaint | ESP complaint webhook |
| `unsubscribe` | User opt-out | Unsubscribe link click |
| `spam_trap` | Known honeypot address | Manual/import |
| `role_based` | Generic address (abuse@, postmaster@) | Auto-detection |
| `disposable` | Temporary/throwaway domain | Import validation |
| `known_litigator` | Legal risk address | Manual/import |
| `invalid` | Malformed or syntactically incorrect | Validation |
| `manual` | Manually suppressed | Admin action |

---

## Webhooks & Event Processing

### SparkPost Webhooks

**Endpoint:** `POST /api/mailing/webhooks/sparkpost`

**Supported Events:**
- `message_event.delivery` - Email delivered
- `message_event.bounce` - Email bounced
- `message_event.spam_complaint` - Spam complaint
- `track_event.open` - Email opened
- `track_event.click` - Link clicked
- `unsubscribe_event.link_unsubscribe` - Unsubscribe clicked

**Handler:**
```go
func (svc *MailingService) HandleSparkPostWebhook(w http.ResponseWriter, r *http.Request) {
    var events []SparkPostEvent
    json.NewDecoder(r.Body).Decode(&events)
    
    for _, event := range events {
        switch event.Type {
        case "bounce":
            svc.processBounce(event.Recipient, event.BounceClass, event.Reason)
        case "spam_complaint":
            svc.processComplaint(event.Recipient)
        case "delivery":
            svc.recordDelivery(event.CampaignID, event.Recipient)
        }
    }
    
    w.WriteHeader(http.StatusOK)
}
```

### AWS SES Webhooks (via SNS)

**Endpoint:** `POST /api/mailing/webhooks/ses`

**Supported Notification Types:**
- `Bounce` - Hard/soft bounces
- `Complaint` - Spam complaints
- `Delivery` - Successful delivery

**Handler:**
```go
func (svc *MailingService) HandleSESWebhook(w http.ResponseWriter, r *http.Request) {
    var notification SNSNotification
    json.NewDecoder(r.Body).Decode(&notification)
    
    // Handle subscription confirmation
    if notification.Type == "SubscriptionConfirmation" {
        http.Get(notification.SubscribeURL)
        return
    }
    
    var message SESMessage
    json.Unmarshal([]byte(notification.Message), &message)
    
    switch message.NotificationType {
    case "Bounce":
        for _, recipient := range message.Bounce.BouncedRecipients {
            bounceType := "soft"
            if message.Bounce.BounceType == "Permanent" {
                bounceType = "hard"
            }
            svc.processBounce(recipient.EmailAddress, bounceType, message.Bounce.BounceSubType)
        }
    case "Complaint":
        for _, recipient := range message.Complaint.ComplainedRecipients {
            svc.processComplaint(recipient.EmailAddress)
        }
    }
    
    w.WriteHeader(http.StatusOK)
}
```

### Mailgun Webhooks

**Endpoint:** `POST /api/mailing/webhooks/mailgun`

**Supported Events:**
- `failed` (permanent) - Hard bounce
- `complained` - Spam complaint
- `unsubscribed` - User unsubscribed

**Handler:**
```go
func (svc *MailingService) HandleMailgunWebhook(w http.ResponseWriter, r *http.Request) {
    // Try JSON format first (v2)
    var event MailgunEvent
    if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
        // Fall back to form data (legacy)
        r.ParseForm()
        event.EventType = r.FormValue("event")
        event.Recipient = r.FormValue("recipient")
        event.Severity = r.FormValue("severity")
    }
    
    switch event.EventType {
    case "failed":
        if event.Severity == "permanent" {
            svc.processBounce(event.Recipient, "hard", event.Reason)
        }
    case "complained":
        svc.processComplaint(event.Recipient)
    case "unsubscribed":
        svc.processUnsubscribe(event.Recipient)
    }
    
    w.WriteHeader(http.StatusOK)
}
```

### Bounce Processing

```go
func (svc *MailingService) processBounce(email, bounceType, reason string) {
    // Determine category
    category := "soft_bounce"
    if bounceType == "hard" {
        category = "hard_bounce"
    }
    
    // Add to global suppression list
    svc.addToGlobalSuppression(email, category, "esp_webhook")
    
    // Update subscriber status
    svc.db.Exec(`
        UPDATE mailing_subscribers 
        SET status = 'bounced', bounce_count = bounce_count + 1
        WHERE LOWER(email) = LOWER($1)
    `, email)
    
    // Record bounce event
    svc.db.Exec(`
        INSERT INTO mailing_tracking_events 
        (id, email, event_type, metadata, event_time)
        VALUES ($1, $2, 'bounced', $3, NOW())
    `, uuid.New(), email, fmt.Sprintf(`{"type":"%s","reason":"%s"}`, bounceType, reason))
}
```

### Complaint Processing

```go
func (svc *MailingService) processComplaint(email string) {
    // Add to global suppression list (CRITICAL - never email again)
    svc.addToGlobalSuppression(email, "spam_complaint", "esp_webhook")
    
    // Update subscriber status
    svc.db.Exec(`
        UPDATE mailing_subscribers 
        SET status = 'complained', complaint_count = complaint_count + 1
        WHERE LOWER(email) = LOWER($1)
    `, email)
    
    // Record complaint event
    svc.db.Exec(`
        INSERT INTO mailing_tracking_events 
        (id, email, event_type, event_time)
        VALUES ($1, $2, 'complained', NOW())
    `, uuid.New(), email)
    
    log.Printf("COMPLAINT: %s added to global suppression", email)
}
```

---

## API Reference

### Campaign Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/mailing/campaigns` | List campaigns |
| POST | `/api/mailing/campaigns` | Create campaign |
| GET | `/api/mailing/campaigns/{id}` | Get campaign |
| PUT | `/api/mailing/campaigns/{id}` | Update campaign |
| DELETE | `/api/mailing/campaigns/{id}` | Delete campaign |
| POST | `/api/mailing/campaigns/{id}/send` | Send immediately |
| POST | `/api/mailing/campaigns/{id}/schedule` | Schedule for later |
| POST | `/api/mailing/campaigns/{id}/pause` | Pause sending |
| POST | `/api/mailing/campaigns/{id}/resume` | Resume sending |
| POST | `/api/mailing/campaigns/{id}/cancel` | Cancel campaign |
| POST | `/api/mailing/campaigns/{id}/reset` | Reset to draft |
| POST | `/api/mailing/campaigns/{id}/duplicate` | Duplicate campaign |
| POST | `/api/mailing/campaigns/{id}/test` | Send test email |
| GET | `/api/mailing/campaigns/{id}/preview` | HTML preview |
| GET | `/api/mailing/campaigns/{id}/stats` | Get statistics |
| GET | `/api/mailing/campaigns/{id}/estimate` | Estimate audience |

### Create Campaign Request

```json
POST /api/mailing/campaigns
{
  "name": "February Newsletter",
  "subject": "Your February Update",
  "preview_text": "What's new this month...",
  "html_content": "<html>...</html>",
  "text_content": "Plain text version...",
  "list_id": "uuid",
  "segment_ids": ["uuid1", "uuid2"],
  "suppression_list_ids": ["uuid1", "uuid2"],
  "sending_profile_id": "uuid",
  "from_name": "IGNITE News",
  "from_email": "news@newsletter.domain.com",
  "reply_email": "support@domain.com",
  "send_type": "scheduled",
  "scheduled_at": "2026-02-03T13:00:00Z",
  "throttle_speed": "gentle",
  "max_recipients": 10000
}
```

### Send Campaign Response

```json
{
  "campaign_id": "uuid",
  "status": "completed",
  "sent": 9950,
  "failed": 5,
  "suppressed": 45,
  "total_targeted": 10000,
  "vendor": "sparkpost",
  "throttle_speed": "gentle"
}
```

### Schedule Campaign Request

```json
POST /api/mailing/campaigns/{id}/schedule
{
  "send_at": "2026-02-03T13:15:00Z",
  "timezone": "America/Denver"
}
```

---

## Configuration Guide

### Environment Variables

```bash
# SparkPost
SPARKPOST_API_KEY=your-sparkpost-api-key

# AWS SES (uses default AWS credentials)
AWS_REGION=us-east-1
AWS_PROFILE=default

# Mailgun
MAILGUN_API_KEY=your-mailgun-api-key
MAILGUN_DOMAIN=mg.yourdomain.com

# SendGrid
SENDGRID_API_KEY=your-sendgrid-api-key

# Database
DATABASE_URL=postgres://user:pass@localhost:5432/mailing

# Tracking
TRACKING_DOMAIN=track.yourdomain.com
BASE_URL=https://api.yourdomain.com
```

### DNS Configuration

For each sending domain, configure:

**SPF Record:**
```
v=spf1 include:sparkpostmail.com include:amazonses.com include:mailgun.org ~all
```

**DKIM Record:**
```
selector._domainkey.yourdomain.com IN TXT "v=DKIM1; k=rsa; p=..."
```

**DMARC Record:**
```
_dmarc.yourdomain.com IN TXT "v=DMARC1; p=quarantine; rua=mailto:dmarc@yourdomain.com"
```

### Recommended Throttle Settings by Use Case

| Use Case | Preset | Rate | Notes |
|----------|--------|------|-------|
| Transactional | `instant` | 1000/min | Password resets, receipts |
| Newsletter | `gentle` | 100/min | Regular newsletters |
| Promotional | `moderate` | 50/min | Marketing campaigns |
| Re-engagement | `careful` | 20/min | Cold list warming |
| New Domain | `careful` | 20/min | IP/domain warming |
| High Volume | `custom` | Based on ESP limits | Enterprise sending |

### Monitoring & Alerts

**Key Metrics to Monitor:**
- Bounce rate (target: < 3%)
- Complaint rate (target: < 0.1%)
- Open rate (benchmark: 15-25%)
- Click rate (benchmark: 2-5%)
- Delivery rate (target: > 95%)

**Alert Thresholds:**
```go
var AlertThresholds = map[string]float64{
    "bounce_rate_warning":    0.02,  // 2%
    "bounce_rate_critical":   0.05,  // 5%
    "complaint_rate_warning": 0.001, // 0.1%
    "complaint_rate_critical":0.002, // 0.2%
    "delivery_rate_warning":  0.95,  // 95%
    "delivery_rate_critical": 0.90,  // 90%
}
```

---

## Best Practices

### Sender Reputation

1. **Warm up new IPs/domains** - Use `careful` throttle for first 2-4 weeks
2. **Maintain list hygiene** - Remove bounces/complaints immediately
3. **Honor unsubscribes** - Process within 24 hours
4. **Monitor feedback loops** - Integrate all ESP complaint webhooks
5. **Authenticate emails** - SPF, DKIM, DMARC on all sending domains

### Deliverability

1. **Segment engaged subscribers** - Mail engaged users more frequently
2. **Re-engage or sunset inactive** - Remove users inactive > 6 months
3. **Avoid spam trigger words** - Review content for spam signals
4. **Test before sending** - Use preview and test send features
5. **Monitor blacklists** - Check MXToolbox, Spamhaus regularly

### Performance

1. **Use appropriate throttle** - Don't exceed ESP rate limits
2. **Schedule large campaigns** - Spread across time windows
3. **Pre-warm suppression engine** - Load lists before send
4. **Monitor queue depth** - Track pending sends
5. **Scale horizontally** - Multiple sending workers for high volume

---

*Last Updated: February 2026*
*Version: 1.0*
