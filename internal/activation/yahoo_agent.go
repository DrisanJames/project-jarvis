package activation

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ============================================================================
// YAHOO DATA ACTIVATION AGENT
// Specialized agent for Yahoo inbox activation with audience profiling,
// adaptive sending, and A/B test management.
// ============================================================================

// YahooActivationAgent manages Yahoo-focused data activation campaigns
type YahooActivationAgent struct {
	Config           YahooAgentConfig
	AudienceProfiles []YahooAudienceProfile
	ABTestPlan       ABTestPlan
	WarmupSchedule   []WarmupTier
	CurrentState     AgentState
}

// YahooAgentConfig holds campaign configuration
type YahooAgentConfig struct {
	SendingDomain     string   `json:"sending_domain"`
	ESP               string   `json:"esp"` // sparkpost
	TotalRecords      int      `json:"total_records"`
	TargetOpenRate    float64  `json:"target_open_rate"` // 0.05 = 5%
	MaxComplaintRate  float64  `json:"max_complaint_rate"` // 0.0008 = 0.08%
	SubjectLines      []string `json:"subject_lines"`
	FromNames         []string `json:"from_names"`
	CreativeHTML      string   `json:"creative_html"`
	SuppressionListID string   `json:"suppression_list_id"`
	SeedListID        string   `json:"seed_list_id"`
	DryRun            bool     `json:"dry_run"`
}

// YahooAudienceProfile represents a profiled Yahoo audience member
type YahooAudienceProfile struct {
	Email           string    `json:"email"`
	EngagementTier  string    `json:"engagement_tier"` // seed, hot, warm, cold, unknown
	LastOpenDate    *time.Time `json:"last_open_date,omitempty"`
	LastClickDate   *time.Time `json:"last_click_date,omitempty"`
	TotalSends      int       `json:"total_sends"`
	TotalOpens      int       `json:"total_opens"`
	TotalClicks     int       `json:"total_clicks"`
	BounceCount     int       `json:"bounce_count"`
	ComplaintCount  int       `json:"complaint_count"`
	EngagementScore float64   `json:"engagement_score"` // 0-100
	RiskLevel       string    `json:"risk_level"` // low, medium, high
	RecommendedBatch int      `json:"recommended_batch"` // 1, 2, 3... which send batch
}

// ABTestPlan defines the A/B test structure
type ABTestPlan struct {
	Variants        []ABVariant `json:"variants"`
	SeedBatchSize   int         `json:"seed_batch_size"`
	TestBatchSize   int         `json:"test_batch_size"`
	WinnerCriteria  string      `json:"winner_criteria"` // open_rate
	WinnerThreshold float64     `json:"winner_threshold"` // minimum open rate
	MinSampleSize   int         `json:"min_sample_size"`
	TestDuration    string      `json:"test_duration"` // e.g., "4h"
	Status          string      `json:"status"` // planned, testing, winner_selected, completed
	WinnerID        string      `json:"winner_id,omitempty"`
}

// ABVariant represents a single A/B test variant
type ABVariant struct {
	ID            string  `json:"id"`
	SubjectLine   string  `json:"subject_line"`
	FromName      string  `json:"from_name"`
	PreheaderText string  `json:"preheader_text"`
	SplitPct      float64 `json:"split_pct"` // percentage of test batch
	Sent          int     `json:"sent"`
	Opens         int     `json:"opens"`
	Clicks        int     `json:"clicks"`
	Bounces       int     `json:"bounces"`
	Complaints    int     `json:"complaints"`
	OpenRate      float64 `json:"open_rate"`
	ClickRate     float64 `json:"click_rate"`
	IsWinner      bool    `json:"is_winner"`
}

// WarmupTier defines a sending tier in the warmup schedule
type WarmupTier struct {
	Tier         int    `json:"tier"`
	Name         string `json:"name"` // Seed, Hot Openers, Warm, Cold
	Volume       int    `json:"volume"`
	RatePerMin   int    `json:"rate_per_min"`
	DelayMinutes int    `json:"delay_minutes"` // delay before starting this tier
	Segment      string `json:"segment"`
	Status       string `json:"status"` // pending, sending, completed, paused
}

// AgentState tracks the current state of the activation
type AgentState struct {
	Phase            string    `json:"phase"` // preparing, seed_testing, ab_testing, ramping, full_send, completed, paused
	CurrentTier      int       `json:"current_tier"`
	TotalSent        int       `json:"total_sent"`
	TotalOpens       int       `json:"total_opens"`
	TotalClicks      int       `json:"total_clicks"`
	TotalBounces     int       `json:"total_bounces"`
	TotalComplaints  int       `json:"total_complaints"`
	CurrentOpenRate  float64   `json:"current_open_rate"`
	CurrentBounceRate float64  `json:"current_bounce_rate"`
	ComplaintRate    float64   `json:"complaint_rate"`
	InboxRate        float64   `json:"inbox_rate"` // from eDataSource
	ThrottleRate     int       `json:"throttle_rate"` // current msgs/min
	IsPaused         bool      `json:"is_paused"`
	PauseReason      string    `json:"pause_reason,omitempty"`
	LastUpdated      time.Time `json:"last_updated"`
	Alerts           []Alert   `json:"alerts"`
}

// Alert represents an agent alert or action taken
type Alert struct {
	Time     time.Time `json:"time"`
	Level    string    `json:"level"` // info, warning, critical
	Message  string    `json:"message"`
	Action   string    `json:"action,omitempty"` // action taken automatically
}

// ============================================================================
// CONSTRUCTOR
// ============================================================================

// NewYahooActivationAgent creates a new Yahoo activation agent with the given config
func NewYahooActivationAgent(config YahooAgentConfig) *YahooActivationAgent {
	agent := &YahooActivationAgent{
		Config: config,
		CurrentState: AgentState{
			Phase:       "preparing",
			LastUpdated: time.Now(),
			Alerts:      []Alert{},
		},
	}

	// Generate A/B test plan from subject lines and from names
	agent.ABTestPlan = agent.generateABTestPlan()

	// Generate warmup schedule
	agent.WarmupSchedule = agent.generateWarmupSchedule()

	return agent
}

// ============================================================================
// AUDIENCE PROFILING
// ============================================================================

// ProfileAudience analyzes the audience and assigns engagement tiers
// This is the repeatable process for any new Yahoo data activation
func (a *YahooActivationAgent) ProfileAudience(emails []string, engagementData map[string]EngagementData) {
	profiles := make([]YahooAudienceProfile, 0, len(emails))

	for _, email := range emails {
		profile := YahooAudienceProfile{
			Email: email,
		}

		// Look up engagement data if available
		if data, ok := engagementData[email]; ok {
			profile.TotalSends = data.TotalSends
			profile.TotalOpens = data.TotalOpens
			profile.TotalClicks = data.TotalClicks
			profile.BounceCount = data.BounceCount
			profile.ComplaintCount = data.ComplaintCount
			profile.LastOpenDate = data.LastOpenDate
			profile.LastClickDate = data.LastClickDate
		}

		// Calculate engagement score (0-100)
		profile.EngagementScore = calculateEngagementScore(profile)

		// Assign tier based on score
		profile.EngagementTier = assignEngagementTier(profile)

		// Assign risk level
		profile.RiskLevel = assessRisk(profile)

		// Assign recommended batch
		profile.RecommendedBatch = assignBatch(profile)

		profiles = append(profiles, profile)
	}

	a.AudienceProfiles = profiles
}

// EngagementData holds historical engagement for profiling
type EngagementData struct {
	TotalSends     int
	TotalOpens     int
	TotalClicks    int
	BounceCount    int
	ComplaintCount int
	LastOpenDate   *time.Time
	LastClickDate  *time.Time
}

// GetAudienceBreakdown returns tier distribution
func (a *YahooActivationAgent) GetAudienceBreakdown() map[string]int {
	breakdown := map[string]int{
		"seed":    0,
		"hot":     0,
		"warm":    0,
		"cold":    0,
		"unknown": 0,
	}
	for _, p := range a.AudienceProfiles {
		breakdown[p.EngagementTier]++
	}
	return breakdown
}

// GetActivationSnapshot returns the complete state for the UI dashboard
func (a *YahooActivationAgent) GetActivationSnapshot() map[string]interface{} {
	return map[string]interface{}{
		"config":             a.Config,
		"audience_breakdown": a.GetAudienceBreakdown(),
		"audience_count":     len(a.AudienceProfiles),
		"ab_test_plan":       a.ABTestPlan,
		"warmup_schedule":    a.WarmupSchedule,
		"current_state":      a.CurrentState,
		"yahoo_best_practices": getYahooBestPractices(),
	}
}

// ============================================================================
// A/B TEST PLAN GENERATION
// ============================================================================

func (a *YahooActivationAgent) generateABTestPlan() ABTestPlan {
	variants := []ABVariant{}

	// Select top 8 combinations from subject lines × from names
	// Strategy: pair each subject with a different from name for variety
	numVariants := 8
	if len(a.Config.SubjectLines) < numVariants {
		numVariants = len(a.Config.SubjectLines)
	}

	preheaders := generatePreheaderVariations()

	for i := 0; i < numVariants; i++ {
		subjectIdx := i % len(a.Config.SubjectLines)
		fromIdx := i % len(a.Config.FromNames)
		preheaderIdx := i % len(preheaders)

		variants = append(variants, ABVariant{
			ID:            fmt.Sprintf("variant_%s", string(rune('A'+i))),
			SubjectLine:   a.Config.SubjectLines[subjectIdx],
			FromName:      a.Config.FromNames[fromIdx],
			PreheaderText: preheaders[preheaderIdx],
			SplitPct:      100.0 / float64(numVariants),
		})
	}

	// Seed batch = 2% of total, test batch = 10% of total
	seedBatch := int(math.Ceil(float64(a.Config.TotalRecords) * 0.02))
	testBatch := int(math.Ceil(float64(a.Config.TotalRecords) * 0.10))

	return ABTestPlan{
		Variants:        variants,
		SeedBatchSize:   seedBatch,
		TestBatchSize:   testBatch,
		WinnerCriteria:  "open_rate",
		WinnerThreshold: a.Config.TargetOpenRate,
		MinSampleSize:   500,
		TestDuration:    "4h",
		Status:          "planned",
	}
}

func generatePreheaderVariations() []string {
	return []string{
		"Limited time offer — join today and save big",
		"Your membership practically pays for itself",
		"Shop smarter with Sam's Club savings",
		"Don't miss this exclusive membership deal",
		"Save $30 on your Sam's Club membership today",
		"Warehouse prices + $30 cash back = unbeatable",
		"New members get $30 Sam's Cash instantly",
		"The smartest shopping decision you'll make today",
	}
}

// ============================================================================
// WARMUP SCHEDULE
// ============================================================================

func (a *YahooActivationAgent) generateWarmupSchedule() []WarmupTier {
	total := a.Config.TotalRecords

	// Yahoo-specific warmup: conservative start, aggressive ramp on good signals
	return []WarmupTier{
		{Tier: 1, Name: "Seed Test", Volume: int(math.Min(float64(total)*0.02, 3000)), RatePerMin: 100, DelayMinutes: 0, Segment: "seed_engaged", Status: "pending"},
		{Tier: 2, Name: "Hot Openers", Volume: int(math.Min(float64(total)*0.08, 15000)), RatePerMin: 250, DelayMinutes: 120, Segment: "hot", Status: "pending"},
		{Tier: 3, Name: "Warm Engaged", Volume: int(float64(total) * 0.20), RatePerMin: 500, DelayMinutes: 240, Segment: "warm", Status: "pending"},
		{Tier: 4, Name: "General Audience", Volume: int(float64(total) * 0.40), RatePerMin: 750, DelayMinutes: 360, Segment: "cold", Status: "pending"},
		{Tier: 5, Name: "Remaining Volume", Volume: total, RatePerMin: 1000, DelayMinutes: 480, Segment: "all_remaining", Status: "pending"},
	}
}

// ============================================================================
// ADAPTIVE DECISION ENGINE
// ============================================================================

// EvaluateSignals processes real-time signals and decides on actions
func (a *YahooActivationAgent) EvaluateSignals(sent, opens, clicks, bounces, complaints int, inboxRate float64) []Alert {
	alerts := []Alert{}
	now := time.Now()

	// Update state
	a.CurrentState.TotalSent = sent
	a.CurrentState.TotalOpens = opens
	a.CurrentState.TotalClicks = clicks
	a.CurrentState.TotalBounces = bounces
	a.CurrentState.TotalComplaints = complaints
	a.CurrentState.InboxRate = inboxRate
	a.CurrentState.LastUpdated = now

	if sent > 0 {
		a.CurrentState.CurrentOpenRate = float64(opens) / float64(sent) * 100
		a.CurrentState.CurrentBounceRate = float64(bounces) / float64(sent) * 100
		a.CurrentState.ComplaintRate = float64(complaints) / float64(sent) * 100
	}

	// CRITICAL: Complaint rate > 0.08% → PAUSE immediately
	if a.CurrentState.ComplaintRate > 0.08 {
		a.CurrentState.IsPaused = true
		a.CurrentState.PauseReason = "complaint_rate_exceeded"
		alerts = append(alerts, Alert{
			Time:    now,
			Level:   "critical",
			Message: fmt.Sprintf("Complaint rate %.3f%% exceeds Yahoo 0.08%% threshold — PAUSED", a.CurrentState.ComplaintRate),
			Action:  "campaign_paused",
		})
	}

	// WARNING: Bounce rate > 3% → reduce throttle
	if a.CurrentState.CurrentBounceRate > 3.0 && !a.CurrentState.IsPaused {
		newRate := a.CurrentState.ThrottleRate / 2
		if newRate < 50 {
			newRate = 50
		}
		a.CurrentState.ThrottleRate = newRate
		alerts = append(alerts, Alert{
			Time:    now,
			Level:   "warning",
			Message: fmt.Sprintf("Bounce rate %.1f%% — throttle reduced to %d/min", a.CurrentState.CurrentBounceRate, newRate),
			Action:  "throttle_reduced",
		})
	}

	// WARNING: Yahoo inbox rate < 60% → reduce throttle, consider pausing
	if inboxRate > 0 && inboxRate < 60 {
		alerts = append(alerts, Alert{
			Time:    now,
			Level:   "warning",
			Message: fmt.Sprintf("Yahoo inbox rate %.1f%% is below 60%% — reputation at risk", inboxRate),
			Action:  "throttle_reduced",
		})
	}

	// POSITIVE: Open rate on track → can increase throughput
	if a.CurrentState.CurrentOpenRate >= 5.0 && a.CurrentState.ComplaintRate < 0.05 && a.CurrentState.CurrentBounceRate < 2.0 {
		if a.CurrentState.ThrottleRate < 1000 {
			a.CurrentState.ThrottleRate = int(math.Min(float64(a.CurrentState.ThrottleRate)*1.25, 1000))
			alerts = append(alerts, Alert{
				Time:    now,
				Level:   "info",
				Message: fmt.Sprintf("Open rate %.1f%% on target — throttle increased to %d/min", a.CurrentState.CurrentOpenRate, a.CurrentState.ThrottleRate),
				Action:  "throttle_increased",
			})
		}
	}

	a.CurrentState.Alerts = append(a.CurrentState.Alerts, alerts...)
	return alerts
}

// ============================================================================
// YAHOO BEST PRACTICES (embedded knowledge)
// ============================================================================

func getYahooBestPractices() map[string]interface{} {
	return map[string]interface{}{
		"complaint_rate_max":   "0.08% (Yahoo CFL threshold)",
		"warmup_strategy":     "Start with most engaged 2%, ramp 2x every 2 hours on clean signals",
		"throttle_initial":    "100-250/min for cold domain, 500-1000/min for warm",
		"authentication":      "SPF + DKIM + DMARC required; ensure alignment",
		"feedback_loop":       "Yahoo CFL (Complaint Feedback Loop) must be registered",
		"content_guidelines":  []string{
			"Clear unsubscribe link (1-click preferred)",
			"Avoid URL shorteners",
			"Keep image-to-text ratio balanced",
			"Include physical mailing address",
			"Avoid excessive punctuation in subject lines",
		},
		"engagement_signals":  []string{
			"Opens within first 30 minutes are strongest signal",
			"Clicks indicate high engagement — Yahoo rewards this",
			"Reply-to interactions boost reputation significantly",
			"Folder moves (spam→inbox) are critical reputation signals",
		},
		"recovery_protocol":   []string{
			"If spamming: immediately pause for 2+ hours",
			"Reduce volume by 50% on resume",
			"Send only to most engaged segment",
			"Monitor eDataSource inbox rate before ramping",
			"Consider switching subject line/from name",
		},
	}
}

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

func calculateEngagementScore(p YahooAudienceProfile) float64 {
	score := 0.0

	// Open rate component (40% weight)
	if p.TotalSends > 0 {
		openRate := float64(p.TotalOpens) / float64(p.TotalSends)
		score += openRate * 40
	}

	// Click rate component (30% weight)
	if p.TotalSends > 0 {
		clickRate := float64(p.TotalClicks) / float64(p.TotalSends)
		score += clickRate * 30 * 5 // clicks are 5x more valuable
	}

	// Recency component (20% weight)
	if p.LastOpenDate != nil {
		daysSinceOpen := time.Since(*p.LastOpenDate).Hours() / 24
		if daysSinceOpen < 7 {
			score += 20
		} else if daysSinceOpen < 14 {
			score += 15
		} else if daysSinceOpen < 30 {
			score += 10
		} else if daysSinceOpen < 90 {
			score += 5
		}
	}

	// Penalty for complaints/bounces
	if p.ComplaintCount > 0 {
		score -= 30
	}
	if p.BounceCount > 2 {
		score -= 20
	}

	// Clamp to 0-100
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	return math.Round(score*10) / 10
}

func assignEngagementTier(p YahooAudienceProfile) string {
	if p.EngagementScore >= 80 {
		return "hot"
	} else if p.EngagementScore >= 50 {
		return "warm"
	} else if p.EngagementScore >= 20 {
		return "cold"
	}
	return "unknown"
}

func assessRisk(p YahooAudienceProfile) string {
	if p.ComplaintCount > 0 || p.BounceCount > 3 {
		return "high"
	}
	if p.BounceCount > 0 || (p.TotalSends > 10 && p.TotalOpens == 0) {
		return "medium"
	}
	return "low"
}

func assignBatch(p YahooAudienceProfile) int {
	switch p.EngagementTier {
	case "seed":
		return 1
	case "hot":
		return 2
	case "warm":
		return 3
	case "cold":
		return 4
	default:
		return 5
	}
}

// IsYahooEmail checks if an email belongs to Yahoo/AOL
func IsYahooEmail(email string) bool {
	email = strings.ToLower(email)
	yahooDomains := []string{
		"@yahoo.com", "@yahoo.co", "@ymail.com", "@rocketmail.com",
		"@aol.com", "@aim.com", "@yahoo.co.uk", "@yahoo.ca",
		"@yahoo.com.au", "@yahoo.co.in", "@yahoo.fr", "@yahoo.de",
		"@yahoo.co.jp", "@yahoo.com.br", "@att.net", "@sbcglobal.net",
		"@bellsouth.net", "@pacbell.net", "@flash.net", "@nvbell.net",
		"@prodigy.net", "@swbell.net", "@currently.com",
	}
	for _, d := range yahooDomains {
		if strings.HasSuffix(email, d) {
			return true
		}
	}
	return false
}
