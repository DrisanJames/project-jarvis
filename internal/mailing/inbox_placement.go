package mailing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
)

// InboxPlacementService provides inbox placement monitoring and deliverability tools
type InboxPlacementService struct {
	db *sql.DB
}

// NewInboxPlacementService creates a new InboxPlacementService
func NewInboxPlacementService(db *sql.DB) *InboxPlacementService {
	return &InboxPlacementService{db: db}
}

// InboxTestResult represents the result of an inbox placement test
type InboxTestResult struct {
	ID           string      `json:"id"`
	CampaignID   string      `json:"campaign_id"`
	SeedListID   string      `json:"seed_list_id"`
	TestDate     time.Time   `json:"test_date"`
	OverallScore float64     `json:"overall_score"` // 0-100
	InboxRate    float64     `json:"inbox_rate"`    // % in inbox
	SpamRate     float64     `json:"spam_rate"`     // % in spam
	MissingRate  float64     `json:"missing_rate"`  // % not delivered
	ISPResults   []ISPResult `json:"isp_results"`
	Status       string      `json:"status"` // pending, running, completed, failed
	CreatedAt    time.Time   `json:"created_at"`
	CompletedAt  *time.Time  `json:"completed_at,omitempty"`
}

// ISPResult represents inbox placement results for a specific ISP
type ISPResult struct {
	ISP          string  `json:"isp"` // gmail, yahoo, outlook, aol
	InboxCount   int     `json:"inbox_count"`
	SpamCount    int     `json:"spam_count"`
	MissingCount int     `json:"missing_count"`
	InboxRate    float64 `json:"inbox_rate"`
}

// SeedList represents a collection of seed email addresses for testing
type SeedList struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	Seeds     []Seed    `json:"seeds"`
	Provider  string    `json:"provider"` // internal, emailonacid, litmus, glockapps
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Seed represents a single seed email address
type Seed struct {
	Email    string `json:"email"`
	ISP      string `json:"isp"`
	Provider string `json:"provider"` // gmail, outlook, yahoo, etc.
}

// ReputationScore represents the sending reputation for a domain/IP
type ReputationScore struct {
	OrgID           string      `json:"org_id"`
	SendingDomain   string      `json:"sending_domain"`
	IPAddress       string      `json:"ip_address,omitempty"`
	OverallScore    float64     `json:"overall_score"`    // 0-100
	BounceRate      float64     `json:"bounce_rate"`      // %
	ComplaintRate   float64     `json:"complaint_rate"`   // %
	EngagementRate  float64     `json:"engagement_rate"`  // % opens + clicks
	SpamTrapHits    int         `json:"spam_trap_hits"`
	BlacklistStatus []Blacklist `json:"blacklist_status"`
	Trend           string      `json:"trend"` // improving, stable, declining
	LastUpdated     time.Time   `json:"last_updated"`
}

// Blacklist represents a DNS-based blacklist status
type Blacklist struct {
	Name   string `json:"name"`
	Listed bool   `json:"listed"`
	URL    string `json:"url"`
}

// IPWarmupPlan represents an IP warmup schedule
type IPWarmupPlan struct {
	ID            string      `json:"id"`
	OrgID         string      `json:"org_id"`
	IPAddress     string      `json:"ip_address"`
	PlanType      string      `json:"plan_type"` // conservative, aggressive, custom
	StartDate     time.Time   `json:"start_date"`
	CurrentDay    int         `json:"current_day"`
	TotalDays     int         `json:"total_days"`
	DailySchedule []WarmupDay `json:"daily_schedule"`
	Status        string      `json:"status"` // active, paused, completed
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// WarmupDay represents a single day in the warmup schedule
type WarmupDay struct {
	Day           int     `json:"day"`
	TargetVolume  int     `json:"target_volume"`
	ActualVolume  int     `json:"actual_volume"`
	BounceRate    float64 `json:"bounce_rate"`
	ComplaintRate float64 `json:"complaint_rate"`
	Completed     bool    `json:"completed"`
}

// ISPRecommendation provides actionable recommendations for improving deliverability
type ISPRecommendation struct {
	ISP             string   `json:"isp"`
	Issues          []string `json:"issues"`
	Recommendations []string `json:"recommendations"`
	Priority        string   `json:"priority"` // high, medium, low
}

// SeedListProvider interface for different seed list providers
type SeedListProvider interface {
	GetSeeds(ctx context.Context) ([]Seed, error)
	CheckPlacement(ctx context.Context, messageID string) (*ISPResult, error)
}

// Common DNS blacklists to check
var commonBlacklists = []struct {
	Name   string
	Zone   string
	URL    string
}{
	{"Spamhaus ZEN", "zen.spamhaus.org", "https://www.spamhaus.org/lookup/"},
	{"Spamhaus DBL", "dbl.spamhaus.org", "https://www.spamhaus.org/lookup/"},
	{"Barracuda", "b.barracudacentral.org", "https://www.barracudacentral.org/lookups"},
	{"SpamCop", "bl.spamcop.net", "https://www.spamcop.net/bl.shtml"},
	{"SORBS", "dnsbl.sorbs.net", "http://www.sorbs.net/lookup.shtml"},
	{"CBL", "cbl.abuseat.org", "https://www.abuseat.org/lookup.cgi"},
	{"Invaluement", "invalidhelo.ivmSIP24.invaluement.com", "https://www.invaluement.com/lookup/"},
	{"Truncate", "truncate.gbudb.net", "https://www.gbudb.com/truncate/"},
}

// Conservative 30-day warmup schedule
var conservativeWarmupSchedule = []int{
	50, 100, 200, 400, 600, 800, 1000, 1500,
	2000, 3000, 4000, 5000, 7500, 10000, 15000, 20000,
	25000, 30000, 40000, 50000, 65000, 80000, 100000, 125000,
	150000, 175000, 200000, 250000, 300000, 350000,
}

// Aggressive 15-day warmup schedule
var aggressiveWarmupSchedule = []int{
	200, 500, 1000, 2500, 5000, 10000, 20000, 35000,
	50000, 75000, 100000, 150000, 200000, 275000, 350000,
}

// RunSeedTest initiates an inbox placement test using seed emails
func (s *InboxPlacementService) RunSeedTest(ctx context.Context, campaignID, seedListID string) (*InboxTestResult, error) {
	// Get the seed list
	seedList, err := s.getSeedListByID(ctx, seedListID)
	if err != nil {
		return nil, fmt.Errorf("failed to get seed list: %w", err)
	}

	if len(seedList.Seeds) == 0 {
		return nil, fmt.Errorf("seed list has no seeds")
	}

	// Create test result record
	id := uuid.New().String()
	now := time.Now()

	result := &InboxTestResult{
		ID:         id,
		CampaignID: campaignID,
		SeedListID: seedListID,
		TestDate:   now,
		Status:     "pending",
		ISPResults: []ISPResult{},
		CreatedAt:  now,
	}

	// Calculate initial ISP breakdown from seed list
	ispCounts := make(map[string]int)
	for _, seed := range seedList.Seeds {
		ispCounts[seed.ISP]++
	}

	for isp, count := range ispCounts {
		result.ISPResults = append(result.ISPResults, ISPResult{
			ISP:          isp,
			InboxCount:   0,
			SpamCount:    0,
			MissingCount: count,
			InboxRate:    0,
		})
	}

	// Save to database
	ispResultsJSON, err := json.Marshal(result.ISPResults)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ISP results: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_inbox_test_results 
		(id, campaign_id, seed_list_id, test_date, overall_score, inbox_rate, spam_rate, missing_rate, isp_results, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, id, campaignID, seedListID, now, 0, 0, 0, 100, ispResultsJSON, "pending", now)
	if err != nil {
		return nil, fmt.Errorf("failed to create test result: %w", err)
	}

	// In production, this would trigger async processing to:
	// 1. Send test emails to seed addresses
	// 2. Monitor seed inboxes for delivery/placement
	// 3. Update results as data comes in
	// For now, we'll simulate results after a brief delay

	go s.simulateTestResults(result.ID)

	return result, nil
}

// simulateTestResults simulates inbox placement results (mock implementation)
func (s *InboxPlacementService) simulateTestResults(testID string) {
	// Simulate processing delay
	time.Sleep(5 * time.Second)

	ctx := context.Background()

	// Update status to running
	s.db.ExecContext(ctx, `UPDATE mailing_inbox_test_results SET status = 'running' WHERE id = $1`, testID)

	// Simulate more processing
	time.Sleep(3 * time.Second)

	// Generate simulated results
	ispResults := []ISPResult{
		{ISP: "gmail", InboxCount: 18, SpamCount: 1, MissingCount: 1, InboxRate: 90.0},
		{ISP: "outlook", InboxCount: 15, SpamCount: 3, MissingCount: 2, InboxRate: 75.0},
		{ISP: "yahoo", InboxCount: 9, SpamCount: 0, MissingCount: 1, InboxRate: 90.0},
		{ISP: "aol", InboxCount: 4, SpamCount: 1, MissingCount: 0, InboxRate: 80.0},
	}

	totalInbox := 0
	totalSpam := 0
	totalMissing := 0
	for _, r := range ispResults {
		totalInbox += r.InboxCount
		totalSpam += r.SpamCount
		totalMissing += r.MissingCount
	}
	total := totalInbox + totalSpam + totalMissing

	inboxRate := float64(totalInbox) / float64(total) * 100
	spamRate := float64(totalSpam) / float64(total) * 100
	missingRate := float64(totalMissing) / float64(total) * 100
	overallScore := calculateOverallScore(inboxRate, spamRate, missingRate)

	ispResultsJSON, _ := json.Marshal(ispResults)
	now := time.Now()

	s.db.ExecContext(ctx, `
		UPDATE mailing_inbox_test_results 
		SET overall_score = $1, inbox_rate = $2, spam_rate = $3, missing_rate = $4, 
		    isp_results = $5, status = 'completed', completed_at = $6
		WHERE id = $7
	`, overallScore, inboxRate, spamRate, missingRate, ispResultsJSON, now, testID)
}

// GetTestResults retrieves inbox placement test results for a campaign
func (s *InboxPlacementService) GetTestResults(ctx context.Context, campaignID string) ([]InboxTestResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, seed_list_id, test_date, overall_score, inbox_rate, spam_rate, 
		       missing_rate, isp_results, status, created_at, completed_at
		FROM mailing_inbox_test_results
		WHERE campaign_id = $1
		ORDER BY test_date DESC
	`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("failed to query test results: %w", err)
	}
	defer rows.Close()

	var results []InboxTestResult
	for rows.Next() {
		var r InboxTestResult
		var ispResultsJSON []byte
		var completedAt sql.NullTime

		err := rows.Scan(&r.ID, &r.CampaignID, &r.SeedListID, &r.TestDate, &r.OverallScore,
			&r.InboxRate, &r.SpamRate, &r.MissingRate, &ispResultsJSON, &r.Status, &r.CreatedAt, &completedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan test result: %w", err)
		}

		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}

		if err := json.Unmarshal(ispResultsJSON, &r.ISPResults); err != nil {
			r.ISPResults = []ISPResult{}
		}

		results = append(results, r)
	}

	return results, nil
}

// GetReputationScore calculates and returns the sending reputation for an organization
func (s *InboxPlacementService) GetReputationScore(ctx context.Context, orgID string) (*ReputationScore, error) {
	// Get campaign stats for the last 30 days
	var totalSent, totalBounced, totalComplaints, totalOpens, totalClicks int
	err := s.db.QueryRowContext(ctx, `
		SELECT 
			COALESCE(SUM(sent_count), 0),
			COALESCE(SUM(bounce_count), 0),
			COALESCE(SUM(complaint_count), 0),
			COALESCE(SUM(open_count), 0),
			COALESCE(SUM(click_count), 0)
		FROM mailing_campaigns
		WHERE organization_id = $1 AND created_at > NOW() - INTERVAL '30 days'
	`, orgID).Scan(&totalSent, &totalBounced, &totalComplaints, &totalOpens, &totalClicks)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("failed to get campaign stats: %w", err)
	}

	// Get the primary sending domain
	var sendingDomain string
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(
			(SELECT SPLIT_PART(from_email, '@', 2) FROM mailing_campaigns 
			 WHERE organization_id = $1 AND from_email IS NOT NULL AND from_email != ''
			 ORDER BY created_at DESC LIMIT 1),
			'unknown.com'
		)
	`, orgID).Scan(&sendingDomain)

	// Calculate rates
	var bounceRate, complaintRate, engagementRate float64
	if totalSent > 0 {
		bounceRate = float64(totalBounced) / float64(totalSent) * 100
		complaintRate = float64(totalComplaints) / float64(totalSent) * 100
		engagementRate = float64(totalOpens+totalClicks) / float64(totalSent) * 100
	}

	// Check blacklists for the sending domain
	blacklistStatus, _ := s.CheckBlacklists(ctx, sendingDomain)

	// Calculate overall score
	overallScore := calculateReputationScore(bounceRate, complaintRate, engagementRate, blacklistStatus)

	// Determine trend based on historical data
	trend := s.calculateTrend(ctx, orgID)

	// Get spam trap hits (simulated - would come from feedback loops in production)
	spamTrapHits := 0

	return &ReputationScore{
		OrgID:           orgID,
		SendingDomain:   sendingDomain,
		OverallScore:    overallScore,
		BounceRate:      math.Round(bounceRate*100) / 100,
		ComplaintRate:   math.Round(complaintRate*1000) / 1000,
		EngagementRate:  math.Round(engagementRate*100) / 100,
		SpamTrapHits:    spamTrapHits,
		BlacklistStatus: blacklistStatus,
		Trend:           trend,
		LastUpdated:     time.Now(),
	}, nil
}

// CheckBlacklists checks if an IP or domain is listed on common blacklists
func (s *InboxPlacementService) CheckBlacklists(ctx context.Context, ipOrDomain string) ([]Blacklist, error) {
	var results []Blacklist

	// Determine if it's an IP or domain
	isIP := net.ParseIP(ipOrDomain) != nil

	for _, bl := range commonBlacklists {
		listed := false

		if isIP {
			// Reverse the IP for DNSBL lookup
			listed = checkDNSBL(reverseIP(ipOrDomain), bl.Zone)
		} else {
			// Domain-based blacklist check
			if strings.Contains(bl.Zone, "dbl") {
				listed = checkDNSBL(ipOrDomain, bl.Zone)
			}
		}

		results = append(results, Blacklist{
			Name:   bl.Name,
			Listed: listed,
			URL:    bl.URL,
		})
	}

	return results, nil
}

// GetISPRecommendations provides deliverability recommendations based on ISP performance
func (s *InboxPlacementService) GetISPRecommendations(ctx context.Context, orgID string) ([]ISPRecommendation, error) {
	// Analyze recent campaign performance by recipient ISP
	// This is a simplified implementation - in production would use more sophisticated analysis

	recommendations := []ISPRecommendation{}

	// Get recent test results for the org
	var lastTestID string
	err := s.db.QueryRowContext(ctx, `
		SELECT tr.id FROM mailing_inbox_test_results tr
		JOIN mailing_seed_lists sl ON tr.seed_list_id = sl.id
		WHERE sl.org_id = $1 AND tr.status = 'completed'
		ORDER BY tr.test_date DESC LIMIT 1
	`, orgID).Scan(&lastTestID)

	if err == sql.ErrNoRows {
		// No test results, provide general recommendations
		recommendations = append(recommendations, ISPRecommendation{
			ISP:             "all",
			Issues:          []string{"No inbox placement tests have been run"},
			Recommendations: []string{"Run an inbox placement test to get ISP-specific recommendations", "Set up seed testing for ongoing monitoring"},
			Priority:        "high",
		})
		return recommendations, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get test results: %w", err)
	}

	// Get the ISP results from the last test
	var ispResultsJSON []byte
	err = s.db.QueryRowContext(ctx, `
		SELECT isp_results FROM mailing_inbox_test_results WHERE id = $1
	`, lastTestID).Scan(&ispResultsJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to get ISP results: %w", err)
	}

	var ispResults []ISPResult
	if err := json.Unmarshal(ispResultsJSON, &ispResults); err != nil {
		return nil, fmt.Errorf("failed to parse ISP results: %w", err)
	}

	// Generate recommendations based on ISP performance
	for _, isp := range ispResults {
		rec := ISPRecommendation{
			ISP:             isp.ISP,
			Issues:          []string{},
			Recommendations: []string{},
			Priority:        "low",
		}

		if isp.InboxRate < 70 {
			rec.Priority = "high"
			rec.Issues = append(rec.Issues, fmt.Sprintf("Low inbox rate: %.1f%%", isp.InboxRate))
		} else if isp.InboxRate < 85 {
			rec.Priority = "medium"
			rec.Issues = append(rec.Issues, fmt.Sprintf("Moderate inbox rate: %.1f%%", isp.InboxRate))
		}

		// ISP-specific recommendations
		switch strings.ToLower(isp.ISP) {
		case "gmail":
			if isp.InboxRate < 85 {
				rec.Recommendations = append(rec.Recommendations,
					"Ensure DKIM and SPF are properly configured",
					"Implement DMARC with p=quarantine or p=reject",
					"Check Google Postmaster Tools for domain reputation",
					"Reduce sending frequency to non-engaged subscribers",
				)
			}
		case "outlook", "microsoft":
			if isp.InboxRate < 85 {
				rec.Recommendations = append(rec.Recommendations,
					"Register with Microsoft SNDS (Smart Network Data Services)",
					"Apply for JMRP (Junk Mail Reporting Program)",
					"Ensure consistent From address and domain",
					"Check for IP reputation issues with Microsoft",
				)
			}
		case "yahoo", "aol":
			if isp.InboxRate < 85 {
				rec.Recommendations = append(rec.Recommendations,
					"Register for Yahoo/AOL feedback loop",
					"Check CFL (Complaint Feedback Loop) status",
					"Ensure clean list hygiene - remove hard bounces immediately",
					"Monitor engagement metrics closely",
				)
			}
		}

		// Add spam folder recommendations if applicable
		total := isp.InboxCount + isp.SpamCount + isp.MissingCount
		if total > 0 {
			spamRate := float64(isp.SpamCount) / float64(total) * 100
			if spamRate > 5 {
				rec.Issues = append(rec.Issues, fmt.Sprintf("High spam folder rate: %.1f%%", spamRate))
				rec.Recommendations = append(rec.Recommendations,
					"Review email content for spam trigger words",
					"Check HTML/text ratio and link density",
					"Ensure unsubscribe link is prominent and functional",
				)
			}

			missingRate := float64(isp.MissingCount) / float64(total) * 100
			if missingRate > 5 {
				rec.Issues = append(rec.Issues, fmt.Sprintf("High missing/blocked rate: %.1f%%", missingRate))
				rec.Recommendations = append(rec.Recommendations,
					"Check if sending IP is blacklisted",
					"Verify DNS records (SPF, DKIM, DMARC)",
					"Review sending patterns for sudden volume spikes",
				)
			}
		}

		if len(rec.Issues) > 0 || len(rec.Recommendations) > 0 {
			recommendations = append(recommendations, rec)
		}
	}

	return recommendations, nil
}

// CreateWarmupPlan creates an IP warmup plan
func (s *InboxPlacementService) CreateWarmupPlan(ctx context.Context, orgID, ip string, targetVolume int, planType string) (*IPWarmupPlan, error) {
	// Validate plan type
	var schedule []int
	var totalDays int

	switch planType {
	case "conservative":
		schedule = conservativeWarmupSchedule
		totalDays = 30
	case "aggressive":
		schedule = aggressiveWarmupSchedule
		totalDays = 15
	case "custom":
		// Generate custom schedule based on target volume
		schedule = generateCustomWarmupSchedule(targetVolume)
		totalDays = len(schedule)
	default:
		return nil, fmt.Errorf("invalid plan type: %s", planType)
	}

	// Build daily schedule
	dailySchedule := make([]WarmupDay, totalDays)
	for i := 0; i < totalDays; i++ {
		volume := schedule[i]
		if i < len(schedule) {
			volume = schedule[i]
		} else {
			volume = schedule[len(schedule)-1]
		}

		// Cap at target volume
		if volume > targetVolume {
			volume = targetVolume
		}

		dailySchedule[i] = WarmupDay{
			Day:           i + 1,
			TargetVolume:  volume,
			ActualVolume:  0,
			BounceRate:    0,
			ComplaintRate: 0,
			Completed:     false,
		}
	}

	// Create the plan
	id := uuid.New().String()
	now := time.Now()

	plan := &IPWarmupPlan{
		ID:            id,
		OrgID:         orgID,
		IPAddress:     ip,
		PlanType:      planType,
		StartDate:     now,
		CurrentDay:    1,
		TotalDays:     totalDays,
		DailySchedule: dailySchedule,
		Status:        "active",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	// Save to database
	scheduleJSON, err := json.Marshal(dailySchedule)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal schedule: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_ip_warmup_plans 
		(id, org_id, ip_address, plan_type, start_date, current_day, total_days, daily_schedule, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, id, orgID, ip, planType, now, 1, totalDays, scheduleJSON, "active", now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create warmup plan: %w", err)
	}

	return plan, nil
}

// GetWarmupProgress retrieves the current warmup plan progress
func (s *InboxPlacementService) GetWarmupProgress(ctx context.Context, planID string) (*IPWarmupPlan, error) {
	var plan IPWarmupPlan
	var scheduleJSON []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, ip_address, plan_type, start_date, current_day, total_days, 
		       daily_schedule, status, created_at, updated_at
		FROM mailing_ip_warmup_plans
		WHERE id = $1
	`, planID).Scan(&plan.ID, &plan.OrgID, &plan.IPAddress, &plan.PlanType, &plan.StartDate,
		&plan.CurrentDay, &plan.TotalDays, &scheduleJSON, &plan.Status, &plan.CreatedAt, &plan.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("warmup plan not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get warmup plan: %w", err)
	}

	if err := json.Unmarshal(scheduleJSON, &plan.DailySchedule); err != nil {
		return nil, fmt.Errorf("failed to parse schedule: %w", err)
	}

	return &plan, nil
}

// UpdateWarmupProgress updates the warmup plan with actual sending data
func (s *InboxPlacementService) UpdateWarmupProgress(ctx context.Context, planID string, sent, bounced, complaints int) error {
	// Get current plan
	plan, err := s.GetWarmupProgress(ctx, planID)
	if err != nil {
		return err
	}

	if plan.Status != "active" {
		return fmt.Errorf("warmup plan is not active")
	}

	// Update current day stats
	dayIndex := plan.CurrentDay - 1
	if dayIndex >= len(plan.DailySchedule) {
		return fmt.Errorf("warmup plan already completed")
	}

	plan.DailySchedule[dayIndex].ActualVolume += sent
	if sent > 0 {
		plan.DailySchedule[dayIndex].BounceRate = float64(bounced) / float64(sent) * 100
		plan.DailySchedule[dayIndex].ComplaintRate = float64(complaints) / float64(sent) * 100
	}

	// Check if day target is met
	if plan.DailySchedule[dayIndex].ActualVolume >= plan.DailySchedule[dayIndex].TargetVolume {
		plan.DailySchedule[dayIndex].Completed = true
		plan.CurrentDay++

		// Check if plan is completed
		if plan.CurrentDay > plan.TotalDays {
			plan.Status = "completed"
		}
	}

	// Check for warning conditions (high bounce/complaint rate)
	if plan.DailySchedule[dayIndex].BounceRate > 5 || plan.DailySchedule[dayIndex].ComplaintRate > 0.1 {
		plan.Status = "paused"
	}

	// Save updated plan
	scheduleJSON, err := json.Marshal(plan.DailySchedule)
	if err != nil {
		return fmt.Errorf("failed to marshal schedule: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_ip_warmup_plans 
		SET current_day = $1, daily_schedule = $2, status = $3, updated_at = $4
		WHERE id = $5
	`, plan.CurrentDay, scheduleJSON, plan.Status, time.Now(), planID)

	return err
}

// CreateSeedList creates a new seed list for inbox testing
func (s *InboxPlacementService) CreateSeedList(ctx context.Context, seedList SeedList) (*SeedList, error) {
	if seedList.Name == "" {
		return nil, fmt.Errorf("seed list name is required")
	}

	if len(seedList.Seeds) == 0 {
		return nil, fmt.Errorf("seed list must contain at least one seed")
	}

	id := uuid.New().String()
	now := time.Now()

	seedList.ID = id
	seedList.CreatedAt = now
	seedList.UpdatedAt = now
	seedList.IsActive = true

	if seedList.Provider == "" {
		seedList.Provider = "internal"
	}

	seedsJSON, err := json.Marshal(seedList.Seeds)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal seeds: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_seed_lists (id, org_id, name, seeds, provider, is_active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, id, seedList.OrgID, seedList.Name, seedsJSON, seedList.Provider, true, now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create seed list: %w", err)
	}

	return &seedList, nil
}

// GetSeedLists retrieves all seed lists for an organization
func (s *InboxPlacementService) GetSeedLists(ctx context.Context, orgID string) ([]SeedList, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, org_id, name, seeds, provider, is_active, created_at, updated_at
		FROM mailing_seed_lists
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to query seed lists: %w", err)
	}
	defer rows.Close()

	var lists []SeedList
	for rows.Next() {
		var sl SeedList
		var seedsJSON []byte

		err := rows.Scan(&sl.ID, &sl.OrgID, &sl.Name, &seedsJSON, &sl.Provider, &sl.IsActive, &sl.CreatedAt, &sl.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan seed list: %w", err)
		}

		if err := json.Unmarshal(seedsJSON, &sl.Seeds); err != nil {
			sl.Seeds = []Seed{}
		}

		lists = append(lists, sl)
	}

	return lists, nil
}

// GetWarmupPlans retrieves all warmup plans for an organization
func (s *InboxPlacementService) GetWarmupPlans(ctx context.Context, orgID string) ([]IPWarmupPlan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, org_id, ip_address, plan_type, start_date, current_day, total_days, 
		       daily_schedule, status, created_at, updated_at
		FROM mailing_ip_warmup_plans
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to query warmup plans: %w", err)
	}
	defer rows.Close()

	var plans []IPWarmupPlan
	for rows.Next() {
		var plan IPWarmupPlan
		var scheduleJSON []byte

		err := rows.Scan(&plan.ID, &plan.OrgID, &plan.IPAddress, &plan.PlanType, &plan.StartDate,
			&plan.CurrentDay, &plan.TotalDays, &scheduleJSON, &plan.Status, &plan.CreatedAt, &plan.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan warmup plan: %w", err)
		}

		if err := json.Unmarshal(scheduleJSON, &plan.DailySchedule); err != nil {
			plan.DailySchedule = []WarmupDay{}
		}

		plans = append(plans, plan)
	}

	return plans, nil
}

// Helper functions

func (s *InboxPlacementService) getSeedListByID(ctx context.Context, id string) (*SeedList, error) {
	var sl SeedList
	var seedsJSON []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, name, seeds, provider, is_active, created_at, updated_at
		FROM mailing_seed_lists
		WHERE id = $1
	`, id).Scan(&sl.ID, &sl.OrgID, &sl.Name, &seedsJSON, &sl.Provider, &sl.IsActive, &sl.CreatedAt, &sl.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("seed list not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get seed list: %w", err)
	}

	if err := json.Unmarshal(seedsJSON, &sl.Seeds); err != nil {
		sl.Seeds = []Seed{}
	}

	return &sl, nil
}

func (s *InboxPlacementService) calculateTrend(ctx context.Context, orgID string) string {
	// Compare last 7 days to previous 7 days
	var recentBounceRate, prevBounceRate float64

	s.db.QueryRowContext(ctx, `
		SELECT CASE WHEN SUM(sent_count) > 0 
			THEN SUM(bounce_count)::float / SUM(sent_count) * 100 
			ELSE 0 END
		FROM mailing_campaigns
		WHERE organization_id = $1 AND created_at > NOW() - INTERVAL '7 days'
	`, orgID).Scan(&recentBounceRate)

	s.db.QueryRowContext(ctx, `
		SELECT CASE WHEN SUM(sent_count) > 0 
			THEN SUM(bounce_count)::float / SUM(sent_count) * 100 
			ELSE 0 END
		FROM mailing_campaigns
		WHERE organization_id = $1 
			AND created_at > NOW() - INTERVAL '14 days' 
			AND created_at <= NOW() - INTERVAL '7 days'
	`, orgID).Scan(&prevBounceRate)

	if prevBounceRate == 0 {
		return "stable"
	}

	change := (recentBounceRate - prevBounceRate) / prevBounceRate * 100
	if change < -10 {
		return "improving"
	} else if change > 10 {
		return "declining"
	}
	return "stable"
}

func calculateOverallScore(inboxRate, spamRate, missingRate float64) float64 {
	// Weight inbox rate heavily, penalize spam and missing
	score := inboxRate - (spamRate * 1.5) - (missingRate * 2.0)
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return math.Round(score*10) / 10
}

func calculateReputationScore(bounceRate, complaintRate, engagementRate float64, blacklists []Blacklist) float64 {
	// Start with base score
	score := 100.0

	// Deduct for high bounce rate (target < 2%)
	if bounceRate > 2 {
		score -= (bounceRate - 2) * 10
	}

	// Deduct heavily for complaint rate (target < 0.1%)
	if complaintRate > 0.1 {
		score -= (complaintRate - 0.1) * 100
	}

	// Bonus for good engagement (> 15% is good)
	if engagementRate > 15 {
		score += (engagementRate - 15) * 0.5
	} else if engagementRate < 10 {
		score -= (10 - engagementRate) * 2
	}

	// Deduct for blacklist listings
	for _, bl := range blacklists {
		if bl.Listed {
			score -= 15
		}
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

func checkDNSBL(query, zone string) bool {
	lookup := fmt.Sprintf("%s.%s", query, zone)
	_, err := net.LookupHost(lookup)
	// If we get a result, the IP/domain is listed
	return err == nil
}

func reverseIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ip
	}
	return fmt.Sprintf("%s.%s.%s.%s", parts[3], parts[2], parts[1], parts[0])
}

func generateCustomWarmupSchedule(targetVolume int) []int {
	// Generate a schedule that ramps up to target volume over ~21 days
	schedule := []int{}
	current := 50

	for current < targetVolume {
		schedule = append(schedule, current)
		// Increase by ~50% each day until we hit target
		current = int(float64(current) * 1.5)
		if current > targetVolume {
			current = targetVolume
		}
	}

	// Add a few days at target volume for stabilization
	for i := 0; i < 3; i++ {
		schedule = append(schedule, targetVolume)
	}

	return schedule
}

// Mock seed list provider implementations

// EmailOnAcidProvider is a mock implementation of the Email on Acid seed testing service
type EmailOnAcidProvider struct {
	apiKey string
}

func NewEmailOnAcidProvider(apiKey string) *EmailOnAcidProvider {
	return &EmailOnAcidProvider{apiKey: apiKey}
}

func (p *EmailOnAcidProvider) GetSeeds(ctx context.Context) ([]Seed, error) {
	// Mock implementation - would integrate with Email on Acid API
	return []Seed{
		{Email: "test_gmail_1@emailonacid.com", ISP: "gmail", Provider: "emailonacid"},
		{Email: "test_gmail_2@emailonacid.com", ISP: "gmail", Provider: "emailonacid"},
		{Email: "test_outlook_1@emailonacid.com", ISP: "outlook", Provider: "emailonacid"},
		{Email: "test_yahoo_1@emailonacid.com", ISP: "yahoo", Provider: "emailonacid"},
	}, nil
}

func (p *EmailOnAcidProvider) CheckPlacement(ctx context.Context, messageID string) (*ISPResult, error) {
	// Mock implementation
	return &ISPResult{
		ISP:          "gmail",
		InboxCount:   9,
		SpamCount:    1,
		MissingCount: 0,
		InboxRate:    90.0,
	}, nil
}

// LitmusProvider is a mock implementation of the Litmus seed testing service
type LitmusProvider struct {
	apiKey string
}

func NewLitmusProvider(apiKey string) *LitmusProvider {
	return &LitmusProvider{apiKey: apiKey}
}

func (p *LitmusProvider) GetSeeds(ctx context.Context) ([]Seed, error) {
	// Mock implementation - would integrate with Litmus API
	return []Seed{
		{Email: "test_gmail@litmus.com", ISP: "gmail", Provider: "litmus"},
		{Email: "test_outlook@litmus.com", ISP: "outlook", Provider: "litmus"},
		{Email: "test_yahoo@litmus.com", ISP: "yahoo", Provider: "litmus"},
	}, nil
}

func (p *LitmusProvider) CheckPlacement(ctx context.Context, messageID string) (*ISPResult, error) {
	// Mock implementation
	return &ISPResult{
		ISP:          "gmail",
		InboxCount:   8,
		SpamCount:    1,
		MissingCount: 1,
		InboxRate:    80.0,
	}, nil
}

// GlockAppsProvider is a mock implementation of the GlockApps seed testing service
type GlockAppsProvider struct {
	apiKey string
}

func NewGlockAppsProvider(apiKey string) *GlockAppsProvider {
	return &GlockAppsProvider{apiKey: apiKey}
}

func (p *GlockAppsProvider) GetSeeds(ctx context.Context) ([]Seed, error) {
	// Mock implementation - would integrate with GlockApps API
	return []Seed{
		{Email: "seed_gmail_1@glockapps.com", ISP: "gmail", Provider: "glockapps"},
		{Email: "seed_gmail_2@glockapps.com", ISP: "gmail", Provider: "glockapps"},
		{Email: "seed_outlook_1@glockapps.com", ISP: "outlook", Provider: "glockapps"},
		{Email: "seed_outlook_2@glockapps.com", ISP: "outlook", Provider: "glockapps"},
		{Email: "seed_yahoo_1@glockapps.com", ISP: "yahoo", Provider: "glockapps"},
		{Email: "seed_aol_1@glockapps.com", ISP: "aol", Provider: "glockapps"},
	}, nil
}

func (p *GlockAppsProvider) CheckPlacement(ctx context.Context, messageID string) (*ISPResult, error) {
	// Mock implementation
	return &ISPResult{
		ISP:          "gmail",
		InboxCount:   17,
		SpamCount:    2,
		MissingCount: 1,
		InboxRate:    85.0,
	}, nil
}
