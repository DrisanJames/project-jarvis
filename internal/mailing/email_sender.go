package mailing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// EmailSender handles sending emails through SparkPost
type EmailSender struct {
	db           *sql.DB
	sparkpostKey string
	baseURL      string
	trackingURL  string
	signingKey   string
	throttler    *Throttler
	profiler     *InboxProfiler
}

// NewEmailSender creates a new email sender
func NewEmailSender(db *sql.DB, sparkpostKey, trackingURL, signingKey string) *EmailSender {
	return &EmailSender{
		db:           db,
		sparkpostKey: sparkpostKey,
		baseURL:      "https://api.sparkpost.com/api/v1",
		trackingURL:  trackingURL,
		signingKey:   signingKey,
		throttler:    NewThrottler(db),
		profiler:     NewInboxProfiler(db),
	}
}

// SendEmail sends a single email through SparkPost
func (es *EmailSender) SendEmail(ctx context.Context, to, fromEmail, fromName, subject, htmlContent, plainContent string, campaignID uuid.UUID) (*SendResult, error) {
	// Check suppression first
	suppressed, reason, err := es.IsEmailSuppressed(ctx, to)
	if err != nil {
		return nil, fmt.Errorf("suppression check failed: %w", err)
	}
	if suppressed {
		return &SendResult{
			Success:    false,
			Suppressed: true,
			Reason:     reason,
		}, nil
	}

	// Check throttling
	if !es.throttler.CanSend(ctx, "sparkpost") {
		return &SendResult{
			Success:   false,
			Throttled: true,
			Reason:    "Rate limit exceeded, email queued",
		}, nil
	}

	// Get or create inbox profile
	profile, err := es.profiler.GetOrCreateProfile(ctx, to)
	if err != nil {
		log.Printf("Warning: Could not get inbox profile for %s: %v", to, err)
	}

	// Generate tracking IDs
	emailID := uuid.New()
	
	// Add tracking pixel and links
	trackedHTML := es.addTracking(htmlContent, campaignID, emailID, to)

	// Build SparkPost transmission
	transmission := map[string]interface{}{
		"recipients": []map[string]interface{}{
			{
				"address": map[string]string{
					"email": to,
				},
			},
		},
		"content": map[string]interface{}{
			"from": map[string]string{
				"email": fromEmail,
				"name":  fromName,
			},
			"subject":   subject,
			"html":      trackedHTML,
			"text":      plainContent,
		},
		"metadata": map[string]interface{}{
			"campaign_id": campaignID.String(),
			"email_id":    emailID.String(),
		},
		"options": map[string]interface{}{
			"open_tracking":  false, // We handle our own tracking
			"click_tracking": false,
		},
	}

	// Send via SparkPost API
	body, _ := json.Marshal(transmission)
	req, err := http.NewRequestWithContext(ctx, "POST", es.baseURL+"/transmissions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", es.sparkpostKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SparkPost API error: %w", err)
	}
	defer resp.Body.Close()

	var spResp struct {
		Results struct {
			TotalAcceptedRecipients int    `json:"total_accepted_recipients"`
			ID                      string `json:"id"`
		} `json:"results"`
		Errors []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	json.NewDecoder(resp.Body).Decode(&spResp)

	if resp.StatusCode != 200 || len(spResp.Errors) > 0 {
		errMsg := "SparkPost error"
		if len(spResp.Errors) > 0 {
			errMsg = spResp.Errors[0].Message
		}
		return &SendResult{
			Success: false,
			Reason:  errMsg,
		}, nil
	}

	// Record the send
	es.throttler.RecordSend(ctx, "sparkpost")
	
	// Update inbox profile
	if profile != nil {
		es.profiler.RecordSend(ctx, to, campaignID, time.Now())
	}

	// Record in database
	_, err = es.db.ExecContext(ctx, `
		INSERT INTO mailing_send_log (id, campaign_id, subscriber_email, sparkpost_id, status, sent_at)
		VALUES ($1, $2, $3, $4, 'sent', NOW())
	`, emailID, campaignID, to, spResp.Results.ID)

	return &SendResult{
		Success:     true,
		MessageID:   spResp.Results.ID,
		EmailID:     emailID.String(),
		SentAt:      time.Now(),
		InboxDomain: extractDomain(to),
	}, nil
}

// SendResult contains the result of sending an email
type SendResult struct {
	Success     bool
	Suppressed  bool
	Throttled   bool
	MessageID   string
	EmailID     string
	Reason      string
	SentAt      time.Time
	InboxDomain string
}

// IsEmailSuppressed checks if an email is on any suppression list
func (es *EmailSender) IsEmailSuppressed(ctx context.Context, email string) (bool, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	
	var reason string
	err := es.db.QueryRowContext(ctx, `
		SELECT reason FROM mailing_suppressions 
		WHERE email = $1 AND active = true
		LIMIT 1
	`, email).Scan(&reason)
	
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	
	return true, reason, nil
}

// AddToSuppressionList adds an email to suppression
func (es *EmailSender) AddToSuppressionList(ctx context.Context, email, reason, source string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	
	_, err := es.db.ExecContext(ctx, `
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at)
		VALUES ($1, $2, $3, $4, true, NOW())
		ON CONFLICT (email) DO UPDATE SET reason = $3, source = $4, active = true, updated_at = NOW()
	`, uuid.New(), email, reason, source)
	
	return err
}

// RemoveFromSuppressionList removes an email from suppression
func (es *EmailSender) RemoveFromSuppressionList(ctx context.Context, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	
	_, err := es.db.ExecContext(ctx, `
		UPDATE mailing_suppressions SET active = false, updated_at = NOW()
		WHERE email = $1
	`, email)
	
	return err
}

// addTracking adds tracking pixel and link tracking to HTML
func (es *EmailSender) addTracking(html string, campaignID, emailID uuid.UUID, email string) string {
	// Generate tracking pixel
	trackData := fmt.Sprintf("%s|%s|%s", campaignID, emailID, email)
	signature := es.signData(trackData)
	trackingPixel := fmt.Sprintf(`<img src="%s/track/open?d=%s&s=%s" width="1" height="1" style="display:none" />`, 
		es.trackingURL, trackData, signature)
	
	// Add pixel before closing body tag
	if strings.Contains(html, "</body>") {
		html = strings.Replace(html, "</body>", trackingPixel+"</body>", 1)
	} else {
		html += trackingPixel
	}
	
	return html
}

func (es *EmailSender) signData(data string) string {
	h := hmac.New(sha256.New, []byte(es.signingKey))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func extractDomain(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 {
		return strings.ToLower(parts[1])
	}
	return ""
}

// Throttler manages send rate limiting
type Throttler struct {
	db           *sql.DB
	mu           sync.RWMutex
	counters     map[string]*RateCounter
	hourlyLimit  int
	minuteLimit  int
}

type RateCounter struct {
	hourly  int
	minute  int
	lastMin time.Time
	lastHr  time.Time
}

func NewThrottler(db *sql.DB) *Throttler {
	return &Throttler{
		db:          db,
		counters:    make(map[string]*RateCounter),
		hourlyLimit: 50000, // 50k/hour default
		minuteLimit: 1000,  // 1k/minute default
	}
}

func (t *Throttler) CanSend(ctx context.Context, serverID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	
	counter, exists := t.counters[serverID]
	if !exists {
		counter = &RateCounter{lastMin: time.Now(), lastHr: time.Now()}
		t.counters[serverID] = counter
	}
	
	now := time.Now()
	
	// Reset minute counter
	if now.Sub(counter.lastMin) > time.Minute {
		counter.minute = 0
		counter.lastMin = now
	}
	
	// Reset hour counter
	if now.Sub(counter.lastHr) > time.Hour {
		counter.hourly = 0
		counter.lastHr = now
	}
	
	// Check limits
	if counter.minute >= t.minuteLimit || counter.hourly >= t.hourlyLimit {
		return false
	}
	
	return true
}

func (t *Throttler) RecordSend(ctx context.Context, serverID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	
	counter := t.counters[serverID]
	if counter != nil {
		counter.minute++
		counter.hourly++
	}
}

func (t *Throttler) SetLimits(hourly, minute int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hourlyLimit = hourly
	t.minuteLimit = minute
}

func (t *Throttler) GetStatus(serverID string) (int, int, int, int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	
	counter := t.counters[serverID]
	if counter == nil {
		return 0, t.minuteLimit, 0, t.hourlyLimit
	}
	return counter.minute, t.minuteLimit, counter.hourly, t.hourlyLimit
}

// InboxProfiler builds and maintains per-inbox intelligence profiles
type InboxProfiler struct {
	db *sql.DB
}

func NewInboxProfiler(db *sql.DB) *InboxProfiler {
	return &InboxProfiler{db: db}
}

// SimpleInboxProfile represents basic intelligence about a specific inbox (used by InboxProfiler)
type SimpleInboxProfile struct {
	Email             string
	Domain            string
	TotalSent         int
	TotalOpens        int
	TotalClicks       int
	TotalBounces      int
	TotalComplaints   int
	EngagementScore   float64
	BestSendHour      int       // Hour of day (0-23) with highest engagement
	BestSendDay       int       // Day of week (0-6) with highest engagement
	LastOpenAt        *time.Time
	LastClickAt       *time.Time
	LastSentAt        *time.Time
	AvgOpenDelayMins  float64   // Average time between send and open
	PreferredSubjects []string  // Subject lines with highest engagement
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (ip *InboxProfiler) GetOrCreateProfile(ctx context.Context, email string) (*SimpleInboxProfile, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	domain := extractDomain(email)
	
	profile := &SimpleInboxProfile{Email: email, Domain: domain}
	
	err := ip.db.QueryRowContext(ctx, `
		SELECT total_sent, total_opens, total_clicks, total_bounces, total_complaints,
			   engagement_score, best_send_hour, best_send_day, last_open_at, last_click_at,
			   last_sent_at, avg_open_delay_mins, created_at, updated_at
		FROM mailing_inbox_profiles
		WHERE email = $1
	`, email).Scan(
		&profile.TotalSent, &profile.TotalOpens, &profile.TotalClicks,
		&profile.TotalBounces, &profile.TotalComplaints, &profile.EngagementScore,
		&profile.BestSendHour, &profile.BestSendDay, &profile.LastOpenAt,
		&profile.LastClickAt, &profile.LastSentAt, &profile.AvgOpenDelayMins,
		&profile.CreatedAt, &profile.UpdatedAt,
	)
	
	if err == sql.ErrNoRows {
		// Create new profile
		_, err = ip.db.ExecContext(ctx, `
			INSERT INTO mailing_inbox_profiles (id, email, domain, engagement_score, created_at, updated_at)
			VALUES ($1, $2, $3, 50.0, NOW(), NOW())
		`, uuid.New(), email, domain)
		if err != nil {
			return nil, err
		}
		profile.EngagementScore = 50.0
		profile.CreatedAt = time.Now()
		profile.UpdatedAt = time.Now()
	} else if err != nil {
		return nil, err
	}
	
	return profile, nil
}

func (ip *InboxProfiler) RecordSend(ctx context.Context, email string, campaignID uuid.UUID, sentAt time.Time) error {
	_, err := ip.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles 
		SET total_sent = total_sent + 1, last_sent_at = $2, updated_at = NOW()
		WHERE email = $1
	`, strings.ToLower(email), sentAt)
	return err
}

func (ip *InboxProfiler) RecordOpen(ctx context.Context, email string, openedAt time.Time) error {
	hour := openedAt.Hour()
	day := int(openedAt.Weekday())
	
	_, err := ip.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles 
		SET total_opens = total_opens + 1, 
			last_open_at = $2,
			best_send_hour = CASE WHEN total_opens = 0 THEN $3 ELSE best_send_hour END,
			best_send_day = CASE WHEN total_opens = 0 THEN $4 ELSE best_send_day END,
			engagement_score = LEAST(100, engagement_score + 2),
			updated_at = NOW()
		WHERE email = $1
	`, strings.ToLower(email), openedAt, hour, day)
	return err
}

func (ip *InboxProfiler) RecordClick(ctx context.Context, email string, clickedAt time.Time) error {
	_, err := ip.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles 
		SET total_clicks = total_clicks + 1, 
			last_click_at = $2,
			engagement_score = LEAST(100, engagement_score + 5),
			updated_at = NOW()
		WHERE email = $1
	`, strings.ToLower(email), clickedAt)
	return err
}

func (ip *InboxProfiler) RecordBounce(ctx context.Context, email string) error {
	_, err := ip.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles 
		SET total_bounces = total_bounces + 1,
			engagement_score = GREATEST(0, engagement_score - 20),
			updated_at = NOW()
		WHERE email = $1
	`, strings.ToLower(email))
	return err
}

func (ip *InboxProfiler) RecordComplaint(ctx context.Context, email string) error {
	_, err := ip.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles 
		SET total_complaints = total_complaints + 1,
			engagement_score = GREATEST(0, engagement_score - 50),
			updated_at = NOW()
		WHERE email = $1
	`, strings.ToLower(email))
	return err
}

// GetBestSendTime analyzes the profile and returns recommended send time
func (ip *InboxProfiler) GetBestSendTime(ctx context.Context, email string) (int, int, float64, error) {
	var bestHour, bestDay int
	var confidence float64 = 0.5
	
	err := ip.db.QueryRowContext(ctx, `
		SELECT best_send_hour, best_send_day, 
			   CASE WHEN total_opens > 10 THEN 0.9
					WHEN total_opens > 5 THEN 0.7
					WHEN total_opens > 0 THEN 0.5
					ELSE 0.3 END as confidence
		FROM mailing_inbox_profiles
		WHERE email = $1
	`, strings.ToLower(email)).Scan(&bestHour, &bestDay, &confidence)
	
	if err == sql.ErrNoRows {
		// Use industry defaults for Gmail
		domain := extractDomain(email)
		switch {
		case strings.Contains(domain, "gmail"):
			return 10, 2, 0.3, nil // Tuesday 10am
		case strings.Contains(domain, "yahoo"):
			return 11, 3, 0.3, nil // Wednesday 11am
		case strings.Contains(domain, "outlook"), strings.Contains(domain, "hotmail"):
			return 9, 1, 0.3, nil // Monday 9am
		default:
			return 10, 2, 0.3, nil // Tuesday 10am default
		}
	}
	
	return bestHour, bestDay, confidence, err
}

// SendingAnalytics provides analytics for decision making
type SendingAnalytics struct {
	db *sql.DB
}

func NewSendingAnalytics(db *sql.DB) *SendingAnalytics {
	return &SendingAnalytics{db: db}
}

// CampaignAnalysis contains analysis of a campaign
type CampaignAnalysis struct {
	CampaignID       string
	Subject          string
	TotalSent        int
	TotalOpens       int
	TotalClicks      int
	OpenRate         float64
	ClickRate        float64
	BestPerformingHr int
	TopDomains       []DomainPerformance
	Recommendations  []string
}

type DomainPerformance struct {
	Domain    string
	Sent      int
	Opens     int
	Clicks    int
	OpenRate  float64
	ClickRate float64
}

func (sa *SendingAnalytics) AnalyzeCampaign(ctx context.Context, campaignID uuid.UUID) (*CampaignAnalysis, error) {
	analysis := &CampaignAnalysis{CampaignID: campaignID.String()}
	
	// Get campaign stats
	err := sa.db.QueryRowContext(ctx, `
		SELECT name, subject, sent_count, open_count, click_count
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&analysis.Subject, &analysis.Subject, &analysis.TotalSent, 
		&analysis.TotalOpens, &analysis.TotalClicks)
	
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	
	if analysis.TotalSent > 0 {
		analysis.OpenRate = float64(analysis.TotalOpens) / float64(analysis.TotalSent) * 100
		analysis.ClickRate = float64(analysis.TotalClicks) / float64(analysis.TotalSent) * 100
	}
	
	// Generate recommendations based on industry knowledge
	analysis.Recommendations = sa.generateRecommendations(analysis)
	
	return analysis, nil
}

func (sa *SendingAnalytics) generateRecommendations(analysis *CampaignAnalysis) []string {
	recs := []string{}
	
	// Open rate recommendations
	if analysis.OpenRate < 10 {
		recs = append(recs, "Open rate below industry average (15-25%). Consider: shorter subject lines, personalization, urgency triggers")
	} else if analysis.OpenRate < 15 {
		recs = append(recs, "Open rate slightly below average. Test: emojis in subject, sender name variations, preheader optimization")
	} else if analysis.OpenRate > 25 {
		recs = append(recs, "Excellent open rate! Consider scaling this subject line pattern to other campaigns")
	}
	
	// Click rate recommendations
	if analysis.ClickRate < 1 {
		recs = append(recs, "Low click rate. Consider: clearer CTAs, above-the-fold placement, reducing link clutter")
	} else if analysis.ClickRate < 2.5 {
		recs = append(recs, "Click rate below average. Test: button colors, CTA copy, mobile optimization")
	} else if analysis.ClickRate > 4 {
		recs = append(recs, "Strong click rate! Analyze what content resonated for future campaigns")
	}
	
	// Click-to-open ratio
	if analysis.TotalOpens > 0 {
		ctor := float64(analysis.TotalClicks) / float64(analysis.TotalOpens) * 100
		if ctor < 10 {
			recs = append(recs, "Low click-to-open ratio. Content may not match subject line promise")
		} else if ctor > 20 {
			recs = append(recs, "Excellent CTOR! Subject line and content alignment is strong")
		}
	}
	
	// Industry best practices
	recs = append(recs, "Industry insight: Tuesday-Thursday 9-11am typically shows highest engagement")
	recs = append(recs, "Industry insight: Mobile opens account for 60%+ - ensure responsive design")
	
	return recs
}

// GetOptimalSendParams returns AI-recommended send parameters
func (sa *SendingAnalytics) GetOptimalSendParams(ctx context.Context, listID uuid.UUID) (*OptimalSendParams, error) {
	params := &OptimalSendParams{
		RecommendedHour:    10,
		RecommendedDay:     2, // Tuesday
		RecommendedSubject: "",
		Confidence:         0.5,
		Reasoning:          []string{},
	}
	
	// Analyze past campaign performance
	rows, err := sa.db.QueryContext(ctx, `
		SELECT c.subject, c.sent_count, c.open_count, c.click_count,
			   EXTRACT(HOUR FROM c.started_at) as send_hour,
			   EXTRACT(DOW FROM c.started_at) as send_day
		FROM mailing_campaigns c
		WHERE c.list_id = $1 AND c.status = 'sent' AND c.sent_count > 0
		ORDER BY (c.open_count::float / NULLIF(c.sent_count, 0)) DESC
		LIMIT 10
	`, listID)
	
	if err != nil {
		return params, err
	}
	defer rows.Close()
	
	var bestSubject string
	var bestOpenRate float64
	var hourCounts [24]int
	var dayCounts [7]int
	var totalCampaigns int
	
	for rows.Next() {
		var subject string
		var sent, opens, clicks int
		var hour, day float64
		
		rows.Scan(&subject, &sent, &opens, &clicks, &hour, &day)
		
		openRate := float64(opens) / float64(sent)
		if openRate > bestOpenRate {
			bestOpenRate = openRate
			bestSubject = subject
		}
		
		hourCounts[int(hour)]++
		dayCounts[int(day)]++
		totalCampaigns++
	}
	
	if totalCampaigns > 0 {
		// Find best hour
		maxHourCount := 0
		for h, count := range hourCounts {
			if count > maxHourCount {
				maxHourCount = count
				params.RecommendedHour = h
			}
		}
		
		// Find best day
		maxDayCount := 0
		for d, count := range dayCounts {
			if count > maxDayCount {
				maxDayCount = count
				params.RecommendedDay = d
			}
		}
		
		params.RecommendedSubject = bestSubject
		params.Confidence = float64(totalCampaigns) / 10.0
		if params.Confidence > 0.9 {
			params.Confidence = 0.9
		}
		
		params.Reasoning = append(params.Reasoning, 
			fmt.Sprintf("Based on %d past campaigns", totalCampaigns))
		params.Reasoning = append(params.Reasoning,
			fmt.Sprintf("Best performing subject had %.1f%% open rate", bestOpenRate*100))
	} else {
		params.Reasoning = append(params.Reasoning, 
			"No historical data - using industry best practices")
		params.Reasoning = append(params.Reasoning,
			"Industry: Tuesday 10am typically optimal for B2C email")
		params.Reasoning = append(params.Reasoning,
			"Consider A/B testing send times once you have baseline data")
	}
	
	return params, nil
}

type OptimalSendParams struct {
	RecommendedHour    int
	RecommendedDay     int
	RecommendedSubject string
	Confidence         float64
	Reasoning          []string
}

// SuggestionEngine handles user suggestions for improvement
type SuggestionEngine struct {
	db *sql.DB
}

func NewSuggestionEngine(db *sql.DB) *SuggestionEngine {
	return &SuggestionEngine{db: db}
}

type Suggestion struct {
	ID          string
	Category    string // "timing", "content", "targeting", "throttling", "creative"
	Description string
	Impact      string
	Status      string // "pending", "accepted", "rejected", "implemented"
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (se *SuggestionEngine) AddSuggestion(ctx context.Context, category, description, impact string) (*Suggestion, error) {
	id := uuid.New()
	
	_, err := se.db.ExecContext(ctx, `
		INSERT INTO mailing_suggestions (id, category, description, impact, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'pending', NOW(), NOW())
	`, id, category, description, impact)
	
	if err != nil {
		return nil, err
	}
	
	return &Suggestion{
		ID:          id.String(),
		Category:    category,
		Description: description,
		Impact:      impact,
		Status:      "pending",
		CreatedAt:   time.Now(),
	}, nil
}

func (se *SuggestionEngine) GetPendingSuggestions(ctx context.Context) ([]Suggestion, error) {
	rows, err := se.db.QueryContext(ctx, `
		SELECT id, category, description, impact, status, created_at, updated_at
		FROM mailing_suggestions
		WHERE status = 'pending'
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var suggestions []Suggestion
	for rows.Next() {
		var s Suggestion
		rows.Scan(&s.ID, &s.Category, &s.Description, &s.Impact, &s.Status, &s.CreatedAt, &s.UpdatedAt)
		suggestions = append(suggestions, s)
	}
	
	return suggestions, nil
}

func (se *SuggestionEngine) UpdateSuggestionStatus(ctx context.Context, id, status string) error {
	_, err := se.db.ExecContext(ctx, `
		UPDATE mailing_suggestions SET status = $2, updated_at = NOW() WHERE id = $1
	`, id, status)
	return err
}

// AnalyticsDecisionEngine combines all analytics for decision making
type AnalyticsDecisionEngine struct {
	db        *sql.DB
	profiler  *InboxProfiler
	analytics *SendingAnalytics
}

func NewAnalyticsDecisionEngine(db *sql.DB) *AnalyticsDecisionEngine {
	return &AnalyticsDecisionEngine{
		db:        db,
		profiler:  NewInboxProfiler(db),
		analytics: NewSendingAnalytics(db),
	}
}

type SendDecision struct {
	ShouldSend       bool
	OptimalHour      int
	OptimalDay       int
	RecommendedDelay time.Duration
	Reasoning        []string
	RiskFactors      []string
	Confidence       float64
}

func (ade *AnalyticsDecisionEngine) MakeSendDecision(ctx context.Context, email string, listID uuid.UUID) (*SendDecision, error) {
	decision := &SendDecision{
		ShouldSend: true,
		Confidence: 0.5,
		Reasoning:  []string{},
		RiskFactors: []string{},
	}
	
	// Get inbox profile intelligence
	profile, err := ade.profiler.GetOrCreateProfile(ctx, email)
	if err != nil {
		return nil, err
	}
	
	// Check engagement score
	if profile.EngagementScore < 20 {
		decision.RiskFactors = append(decision.RiskFactors, 
			fmt.Sprintf("Low engagement score (%.1f) - consider re-engagement campaign first", profile.EngagementScore))
	}
	
	// Check bounce/complaint history
	if profile.TotalBounces > 0 {
		decision.ShouldSend = false
		decision.Reasoning = append(decision.Reasoning, "Previous bounce detected - email may be invalid")
		return decision, nil
	}
	
	if profile.TotalComplaints > 0 {
		decision.ShouldSend = false
		decision.Reasoning = append(decision.Reasoning, "Previous complaint - auto-suppressed")
		return decision, nil
	}
	
	// Get optimal timing from profile
	bestHour, bestDay, confidence, _ := ade.profiler.GetBestSendTime(ctx, email)
	decision.OptimalHour = bestHour
	decision.OptimalDay = bestDay
	decision.Confidence = confidence
	
	// Add reasoning
	if profile.TotalOpens > 0 {
		decision.Reasoning = append(decision.Reasoning,
			fmt.Sprintf("Profile shows %d opens, best engagement at %d:00 on %s", 
				profile.TotalOpens, bestHour, dayName(bestDay)))
	} else {
		decision.Reasoning = append(decision.Reasoning,
			fmt.Sprintf("New inbox - using domain defaults (%s)", profile.Domain))
	}
	
	// Calculate delay if not optimal time
	now := time.Now()
	if now.Hour() != bestHour || int(now.Weekday()) != bestDay {
		decision.RecommendedDelay = calculateDelayToOptimalTime(bestHour, bestDay)
		if decision.RecommendedDelay > 0 {
			decision.Reasoning = append(decision.Reasoning,
				fmt.Sprintf("Recommend delaying %v for optimal engagement", decision.RecommendedDelay.Round(time.Hour)))
		}
	}
	
	return decision, nil
}

func dayName(day int) string {
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	if day >= 0 && day < 7 {
		return days[day]
	}
	return "Unknown"
}

func calculateDelayToOptimalTime(targetHour, targetDay int) time.Duration {
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), targetHour, 0, 0, 0, now.Location())
	
	// Adjust for day of week
	currentDay := int(now.Weekday())
	daysUntil := targetDay - currentDay
	if daysUntil < 0 {
		daysUntil += 7
	}
	if daysUntil == 0 && now.Hour() >= targetHour {
		daysUntil = 7
	}
	
	target = target.AddDate(0, 0, daysUntil)
	
	return target.Sub(now)
}
