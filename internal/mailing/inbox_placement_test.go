package mailing

import (
	"context"
	"testing"
	"time"
)

func TestCalculateOverallScore(t *testing.T) {
	tests := []struct {
		name        string
		inboxRate   float64
		spamRate    float64
		missingRate float64
		wantMin     float64
		wantMax     float64
	}{
		{
			name:        "perfect inbox placement",
			inboxRate:   100,
			spamRate:    0,
			missingRate: 0,
			wantMin:     95,
			wantMax:     100,
		},
		{
			name:        "good inbox placement",
			inboxRate:   90,
			spamRate:    5,
			missingRate: 5,
			wantMin:     70,
			wantMax:     85,
		},
		{
			name:        "moderate spam issues",
			inboxRate:   70,
			spamRate:    20,
			missingRate: 10,
			wantMin:     20,
			wantMax:     50,
		},
		{
			name:        "severe deliverability issues",
			inboxRate:   40,
			spamRate:    30,
			missingRate: 30,
			wantMin:     0,
			wantMax:     20,
		},
		{
			name:        "all spam",
			inboxRate:   0,
			spamRate:    100,
			missingRate: 0,
			wantMin:     0,
			wantMax:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := calculateOverallScore(tt.inboxRate, tt.spamRate, tt.missingRate)
			if score < tt.wantMin || score > tt.wantMax {
				t.Errorf("calculateOverallScore(%v, %v, %v) = %v, want between %v and %v",
					tt.inboxRate, tt.spamRate, tt.missingRate, score, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestCalculateReputationScore(t *testing.T) {
	tests := []struct {
		name           string
		bounceRate     float64
		complaintRate  float64
		engagementRate float64
		blacklists     []Blacklist
		wantMin        float64
		wantMax        float64
	}{
		{
			name:           "excellent reputation",
			bounceRate:     0.5,
			complaintRate:  0.01,
			engagementRate: 25,
			blacklists:     []Blacklist{},
			wantMin:        95,
			wantMax:        100,
		},
		{
			name:           "good reputation",
			bounceRate:     1.5,
			complaintRate:  0.05,
			engagementRate: 18,
			blacklists:     []Blacklist{},
			wantMin:        90,
			wantMax:        100,
		},
		{
			name:           "moderate reputation with bounces",
			bounceRate:     4.0,
			complaintRate:  0.08,
			engagementRate: 15,
			blacklists:     []Blacklist{},
			wantMin:        70,
			wantMax:        90,
		},
		{
			name:           "poor reputation with blacklist",
			bounceRate:     5.0,
			complaintRate:  0.2,
			engagementRate: 10,
			blacklists:     []Blacklist{{Name: "Spamhaus", Listed: true}},
			wantMin:        40,
			wantMax:        70,
		},
		{
			name:           "terrible reputation multiple blacklists",
			bounceRate:     8.0,
			complaintRate:  0.5,
			engagementRate: 5,
			blacklists: []Blacklist{
				{Name: "Spamhaus", Listed: true},
				{Name: "Barracuda", Listed: true},
				{Name: "SpamCop", Listed: true},
			},
			wantMin: 0,
			wantMax: 30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := calculateReputationScore(tt.bounceRate, tt.complaintRate, tt.engagementRate, tt.blacklists)
			if score < tt.wantMin || score > tt.wantMax {
				t.Errorf("calculateReputationScore(%v, %v, %v, blacklists=%d) = %v, want between %v and %v",
					tt.bounceRate, tt.complaintRate, tt.engagementRate, len(tt.blacklists), score, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestReverseIP(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "valid IPv4",
			input:    "192.168.1.1",
			expected: "1.1.168.192",
		},
		{
			name:     "another IPv4",
			input:    "10.0.0.255",
			expected: "255.0.0.10",
		},
		{
			name:     "localhost",
			input:    "127.0.0.1",
			expected: "1.0.0.127",
		},
		{
			name:     "invalid format",
			input:    "not-an-ip",
			expected: "not-an-ip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reverseIP(tt.input)
			if result != tt.expected {
				t.Errorf("reverseIP(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestGenerateCustomWarmupSchedule(t *testing.T) {
	tests := []struct {
		name         string
		targetVolume int
		minDays      int
		maxDays      int
	}{
		{
			name:         "small target",
			targetVolume: 1000,
			minDays:      5,
			maxDays:      15,
		},
		{
			name:         "medium target",
			targetVolume: 50000,
			minDays:      10,
			maxDays:      25,
		},
		{
			name:         "large target",
			targetVolume: 500000,
			minDays:      15,
			maxDays:      35,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schedule := generateCustomWarmupSchedule(tt.targetVolume)

			if len(schedule) < tt.minDays || len(schedule) > tt.maxDays {
				t.Errorf("generateCustomWarmupSchedule(%d) returned %d days, want between %d and %d",
					tt.targetVolume, len(schedule), tt.minDays, tt.maxDays)
			}

			// Verify schedule starts small and ramps up
			if schedule[0] > 100 {
				t.Errorf("Schedule should start with small volume, got %d", schedule[0])
			}

			// Verify schedule reaches target
			lastDay := schedule[len(schedule)-1]
			if lastDay != tt.targetVolume {
				t.Errorf("Schedule should end at target volume %d, got %d", tt.targetVolume, lastDay)
			}

			// Verify schedule is non-decreasing
			for i := 1; i < len(schedule); i++ {
				if schedule[i] < schedule[i-1] {
					t.Errorf("Schedule should be non-decreasing, but day %d (%d) < day %d (%d)",
						i+1, schedule[i], i, schedule[i-1])
				}
			}
		})
	}
}

func TestConservativeWarmupSchedule(t *testing.T) {
	// Test that conservative schedule is 30 days
	if len(conservativeWarmupSchedule) != 30 {
		t.Errorf("Conservative schedule should be 30 days, got %d", len(conservativeWarmupSchedule))
	}

	// Test that it starts small
	if conservativeWarmupSchedule[0] > 100 {
		t.Errorf("Conservative schedule should start with small volume, got %d", conservativeWarmupSchedule[0])
	}

	// Test that it's non-decreasing
	for i := 1; i < len(conservativeWarmupSchedule); i++ {
		if conservativeWarmupSchedule[i] < conservativeWarmupSchedule[i-1] {
			t.Errorf("Schedule should be non-decreasing, but day %d (%d) < day %d (%d)",
				i+1, conservativeWarmupSchedule[i], i, conservativeWarmupSchedule[i-1])
		}
	}
}

func TestAggressiveWarmupSchedule(t *testing.T) {
	// Test that aggressive schedule is 15 days
	if len(aggressiveWarmupSchedule) != 15 {
		t.Errorf("Aggressive schedule should be 15 days, got %d", len(aggressiveWarmupSchedule))
	}

	// Test that it starts higher than conservative
	if aggressiveWarmupSchedule[0] < conservativeWarmupSchedule[0] {
		t.Errorf("Aggressive schedule should start higher than conservative")
	}

	// Test that it's non-decreasing
	for i := 1; i < len(aggressiveWarmupSchedule); i++ {
		if aggressiveWarmupSchedule[i] < aggressiveWarmupSchedule[i-1] {
			t.Errorf("Schedule should be non-decreasing, but day %d (%d) < day %d (%d)",
				i+1, aggressiveWarmupSchedule[i], i, aggressiveWarmupSchedule[i-1])
		}
	}
}

func TestWarmupDay(t *testing.T) {
	day := WarmupDay{
		Day:           1,
		TargetVolume:  1000,
		ActualVolume:  500,
		BounceRate:    1.5,
		ComplaintRate: 0.05,
		Completed:     false,
	}

	if day.Day != 1 {
		t.Errorf("Day = %d, want 1", day.Day)
	}
	if day.TargetVolume != 1000 {
		t.Errorf("TargetVolume = %d, want 1000", day.TargetVolume)
	}
	if day.ActualVolume != 500 {
		t.Errorf("ActualVolume = %d, want 500", day.ActualVolume)
	}
	if day.Completed {
		t.Error("Completed should be false")
	}
}

func TestIPWarmupPlanStructure(t *testing.T) {
	plan := IPWarmupPlan{
		ID:        "test-plan-123",
		OrgID:     "org-123",
		IPAddress: "192.168.1.1",
		PlanType:  "conservative",
		StartDate: time.Now(),
		CurrentDay: 1,
		TotalDays:  30,
		DailySchedule: []WarmupDay{
			{Day: 1, TargetVolume: 50, ActualVolume: 0, Completed: false},
			{Day: 2, TargetVolume: 100, ActualVolume: 0, Completed: false},
		},
		Status:    "active",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if plan.ID != "test-plan-123" {
		t.Errorf("ID = %s, want test-plan-123", plan.ID)
	}
	if plan.PlanType != "conservative" {
		t.Errorf("PlanType = %s, want conservative", plan.PlanType)
	}
	if plan.Status != "active" {
		t.Errorf("Status = %s, want active", plan.Status)
	}
	if len(plan.DailySchedule) != 2 {
		t.Errorf("DailySchedule length = %d, want 2", len(plan.DailySchedule))
	}
}

func TestSeedListStructure(t *testing.T) {
	seedList := SeedList{
		ID:    "seedlist-123",
		OrgID: "org-123",
		Name:  "Test Seed List",
		Seeds: []Seed{
			{Email: "test1@gmail.com", ISP: "gmail", Provider: "internal"},
			{Email: "test2@outlook.com", ISP: "outlook", Provider: "internal"},
			{Email: "test3@yahoo.com", ISP: "yahoo", Provider: "internal"},
		},
		Provider:  "internal",
		IsActive:  true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if seedList.Name != "Test Seed List" {
		t.Errorf("Name = %s, want Test Seed List", seedList.Name)
	}
	if len(seedList.Seeds) != 3 {
		t.Errorf("Seeds length = %d, want 3", len(seedList.Seeds))
	}
	if !seedList.IsActive {
		t.Error("IsActive should be true")
	}

	// Count seeds by ISP
	ispCounts := make(map[string]int)
	for _, seed := range seedList.Seeds {
		ispCounts[seed.ISP]++
	}
	
	if ispCounts["gmail"] != 1 {
		t.Errorf("Gmail seeds = %d, want 1", ispCounts["gmail"])
	}
	if ispCounts["outlook"] != 1 {
		t.Errorf("Outlook seeds = %d, want 1", ispCounts["outlook"])
	}
}

func TestInboxTestResultStructure(t *testing.T) {
	now := time.Now()
	result := InboxTestResult{
		ID:           "test-123",
		CampaignID:   "campaign-456",
		SeedListID:   "seedlist-789",
		TestDate:     now,
		OverallScore: 85.5,
		InboxRate:    90.0,
		SpamRate:     7.5,
		MissingRate:  2.5,
		ISPResults: []ISPResult{
			{ISP: "gmail", InboxCount: 18, SpamCount: 1, MissingCount: 1, InboxRate: 90.0},
			{ISP: "outlook", InboxCount: 8, SpamCount: 2, MissingCount: 0, InboxRate: 80.0},
		},
		Status:    "completed",
		CreatedAt: now,
	}

	if result.OverallScore != 85.5 {
		t.Errorf("OverallScore = %f, want 85.5", result.OverallScore)
	}
	if result.InboxRate != 90.0 {
		t.Errorf("InboxRate = %f, want 90.0", result.InboxRate)
	}
	if result.Status != "completed" {
		t.Errorf("Status = %s, want completed", result.Status)
	}
	if len(result.ISPResults) != 2 {
		t.Errorf("ISPResults length = %d, want 2", len(result.ISPResults))
	}
}

func TestISPResultStructure(t *testing.T) {
	ispResult := ISPResult{
		ISP:          "gmail",
		InboxCount:   90,
		SpamCount:    8,
		MissingCount: 2,
		InboxRate:    90.0,
	}

	total := ispResult.InboxCount + ispResult.SpamCount + ispResult.MissingCount
	if total != 100 {
		t.Errorf("Total count = %d, want 100", total)
	}

	calculatedRate := float64(ispResult.InboxCount) / float64(total) * 100
	if calculatedRate != ispResult.InboxRate {
		t.Errorf("InboxRate = %f, calculated = %f", ispResult.InboxRate, calculatedRate)
	}
}

func TestReputationScoreStructure(t *testing.T) {
	reputation := ReputationScore{
		OrgID:          "org-123",
		SendingDomain:  "example.com",
		IPAddress:      "192.168.1.1",
		OverallScore:   85.0,
		BounceRate:     1.5,
		ComplaintRate:  0.05,
		EngagementRate: 22.5,
		SpamTrapHits:   0,
		BlacklistStatus: []Blacklist{
			{Name: "Spamhaus", Listed: false, URL: "https://spamhaus.org"},
			{Name: "Barracuda", Listed: false, URL: "https://barracuda.com"},
		},
		Trend:       "stable",
		LastUpdated: time.Now(),
	}

	if reputation.OverallScore != 85.0 {
		t.Errorf("OverallScore = %f, want 85.0", reputation.OverallScore)
	}
	if reputation.Trend != "stable" {
		t.Errorf("Trend = %s, want stable", reputation.Trend)
	}
	if len(reputation.BlacklistStatus) != 2 {
		t.Errorf("BlacklistStatus length = %d, want 2", len(reputation.BlacklistStatus))
	}
}

func TestBlacklistStructure(t *testing.T) {
	blacklist := Blacklist{
		Name:   "Spamhaus ZEN",
		Listed: false,
		URL:    "https://www.spamhaus.org/lookup/",
	}

	if blacklist.Name != "Spamhaus ZEN" {
		t.Errorf("Name = %s, want Spamhaus ZEN", blacklist.Name)
	}
	if blacklist.Listed {
		t.Error("Listed should be false")
	}
}

func TestISPRecommendationStructure(t *testing.T) {
	rec := ISPRecommendation{
		ISP:      "gmail",
		Issues:   []string{"Low inbox rate: 72%", "High spam folder rate: 18%"},
		Recommendations: []string{
			"Ensure DKIM and SPF are properly configured",
			"Implement DMARC with p=quarantine or p=reject",
			"Check Google Postmaster Tools for domain reputation",
		},
		Priority: "high",
	}

	if rec.ISP != "gmail" {
		t.Errorf("ISP = %s, want gmail", rec.ISP)
	}
	if rec.Priority != "high" {
		t.Errorf("Priority = %s, want high", rec.Priority)
	}
	if len(rec.Issues) != 2 {
		t.Errorf("Issues length = %d, want 2", len(rec.Issues))
	}
	if len(rec.Recommendations) != 3 {
		t.Errorf("Recommendations length = %d, want 3", len(rec.Recommendations))
	}
}

func TestCommonBlacklists(t *testing.T) {
	// Verify we have the common blacklists defined
	if len(commonBlacklists) < 5 {
		t.Errorf("Should have at least 5 common blacklists, got %d", len(commonBlacklists))
	}

	// Check that each blacklist has required fields
	for i, bl := range commonBlacklists {
		if bl.Name == "" {
			t.Errorf("Blacklist %d has empty name", i)
		}
		if bl.Zone == "" {
			t.Errorf("Blacklist %d (%s) has empty zone", i, bl.Name)
		}
		if bl.URL == "" {
			t.Errorf("Blacklist %d (%s) has empty URL", i, bl.Name)
		}
	}
}

func TestEmailOnAcidProvider(t *testing.T) {
	provider := NewEmailOnAcidProvider("test-api-key")
	
	seeds, err := provider.GetSeeds(context.Background())
	if err != nil {
		t.Errorf("GetSeeds failed: %v", err)
	}
	
	if len(seeds) == 0 {
		t.Error("GetSeeds should return at least one seed")
	}

	// Check that seeds have ISP information
	for _, seed := range seeds {
		if seed.ISP == "" {
			t.Errorf("Seed %s has empty ISP", seed.Email)
		}
		if seed.Provider == "" {
			t.Errorf("Seed %s has empty provider", seed.Email)
		}
	}
}

func TestLitmusProvider(t *testing.T) {
	provider := NewLitmusProvider("test-api-key")
	
	seeds, err := provider.GetSeeds(context.Background())
	if err != nil {
		t.Errorf("GetSeeds failed: %v", err)
	}
	
	if len(seeds) == 0 {
		t.Error("GetSeeds should return at least one seed")
	}
}

func TestGlockAppsProvider(t *testing.T) {
	provider := NewGlockAppsProvider("test-api-key")
	
	seeds, err := provider.GetSeeds(context.Background())
	if err != nil {
		t.Errorf("GetSeeds failed: %v", err)
	}
	
	if len(seeds) == 0 {
		t.Error("GetSeeds should return at least one seed")
	}

	// GlockApps should have more seeds than other providers
	if len(seeds) < 4 {
		t.Errorf("GlockApps should have at least 4 seeds, got %d", len(seeds))
	}
}

func TestProviderCheckPlacement(t *testing.T) {
	providers := []SeedListProvider{
		NewEmailOnAcidProvider("test-key"),
		NewLitmusProvider("test-key"),
		NewGlockAppsProvider("test-key"),
	}

	for _, provider := range providers {
		result, err := provider.CheckPlacement(context.Background(), "test-message-id")
		if err != nil {
			t.Errorf("CheckPlacement failed: %v", err)
		}

		if result.ISP == "" {
			t.Error("ISP should not be empty")
		}
		if result.InboxRate < 0 || result.InboxRate > 100 {
			t.Errorf("InboxRate should be between 0 and 100, got %f", result.InboxRate)
		}
	}
}
