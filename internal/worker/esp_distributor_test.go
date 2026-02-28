package worker

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// ESP DISTRIBUTOR TESTS
// =============================================================================

func setupTestRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	return client, func() {
		client.Close()
		mr.Close()
	}
}

func TestValidateQuotas(t *testing.T) {
	tests := []struct {
		name    string
		quotas  []ESPQuota
		wantErr bool
		errMsg  string
	}{
		{
			name:    "empty quotas",
			quotas:  []ESPQuota{},
			wantErr: true,
			errMsg:  "no ESP quotas configured",
		},
		{
			name: "valid 100% single ESP",
			quotas: []ESPQuota{
				{ProfileID: "profile-1", Percentage: 100},
			},
			wantErr: false,
		},
		{
			name: "valid 60/40 split",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 60},
				{ProfileID: "ses", Percentage: 40},
			},
			wantErr: false,
		},
		{
			name: "valid 33/33/34 split",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 33},
				{ProfileID: "ses", Percentage: 33},
				{ProfileID: "mailgun", Percentage: 34},
			},
			wantErr: false,
		},
		{
			name: "quotas sum to 80%",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 50},
				{ProfileID: "ses", Percentage: 30},
			},
			wantErr: true,
			errMsg:  "must sum to 100%",
		},
		{
			name: "quotas sum to 120%",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 70},
				{ProfileID: "ses", Percentage: 50},
			},
			wantErr: true,
			errMsg:  "must sum to 100%",
		},
		{
			name: "negative percentage",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: -10},
				{ProfileID: "ses", Percentage: 110},
			},
			wantErr: true,
		},
		{
			name: "percentage over 100",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 150},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateQuotas(tt.quotas)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateQuotas() expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("ValidateQuotas() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestESPDistributor_SelectESP(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	ctx := context.Background()
	campaignID := "test-campaign-1"

	tests := []struct {
		name       string
		quotas     []ESPQuota
		setup      func() // Run before selection
		wantErr    bool
		wantESP    string // Expected ESP (empty means any valid)
	}{
		{
			name:    "no quotas configured",
			quotas:  []ESPQuota{},
			wantErr: true,
		},
		{
			name: "single ESP 100%",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 100},
			},
			wantErr: false,
			wantESP: "sparkpost",
		},
		{
			name: "first selection from 60/40 split",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 60},
				{ProfileID: "ses", Percentage: 40},
			},
			wantErr: false,
			// Should pick the one furthest behind quota (both at 0, so first one)
		},
		{
			name: "selection after sends - should balance",
			quotas: []ESPQuota{
				{ProfileID: "sparkpost", Percentage: 50},
				{ProfileID: "ses", Percentage: 50},
			},
			setup: func() {
				// Simulate 10 sends to sparkpost, 0 to ses
				for i := 0; i < 10; i++ {
					distributor.RecordSend(ctx, campaignID, "sparkpost")
				}
			},
			wantErr: false,
			wantESP: "ses", // SES is behind its 50% quota
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear stats
			distributor.ClearStats(ctx, campaignID)

			if tt.setup != nil {
				tt.setup()
			}

			esp, err := distributor.SelectESP(ctx, campaignID, tt.quotas)
			if tt.wantErr {
				if err == nil {
					t.Errorf("SelectESP() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("SelectESP() unexpected error: %v", err)
				return
			}

			if tt.wantESP != "" && esp != tt.wantESP {
				t.Errorf("SelectESP() = %s, want %s", esp, tt.wantESP)
			}
		})
	}
}

func TestESPDistributor_HealthTracking(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	distributor.maxConsecutiveFails = 3
	ctx := context.Background()

	profileID := "test-profile"

	// Initially healthy
	if !distributor.IsHealthy(profileID) {
		t.Error("New profile should be healthy")
	}

	// Record failures
	for i := 0; i < 3; i++ {
		distributor.RecordFailure(ctx, "campaign-1", profileID)
	}

	// Should be unhealthy after 3 consecutive failures
	if distributor.IsHealthy(profileID) {
		t.Error("Profile should be unhealthy after 3 consecutive failures")
	}

	// Record success - should reset consecutive fails
	distributor.RecordSuccess(ctx, profileID)
	
	if !distributor.IsHealthy(profileID) {
		t.Error("Profile should be healthy after success")
	}
}

func TestESPDistributor_Failover(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	distributor.maxConsecutiveFails = 2
	ctx := context.Background()
	campaignID := "test-campaign-failover"

	quotas := []ESPQuota{
		{ProfileID: "primary", Percentage: 80},
		{ProfileID: "backup", Percentage: 20},
	}

	// Clear stats
	distributor.ClearStats(ctx, campaignID)

	// First selection should work
	esp1, err := distributor.SelectESP(ctx, campaignID, quotas)
	if err != nil {
		t.Fatalf("SelectESP() error: %v", err)
	}

	// Simulate failures for primary
	for i := 0; i < 2; i++ {
		distributor.RecordFailure(ctx, campaignID, "primary")
	}

	// Primary should now be unhealthy
	if distributor.IsHealthy("primary") {
		t.Error("Primary should be unhealthy after failures")
	}

	// Next selection should use backup
	esp2, err := distributor.SelectESP(ctx, campaignID, quotas)
	if err != nil {
		t.Fatalf("SelectESP() error after failover: %v", err)
	}

	if esp2 != "backup" {
		t.Errorf("After failover, SelectESP() = %s, want backup", esp2)
	}

	_ = esp1 // esp1 could be either, depending on deficit calculation
}

func TestESPDistributor_AllESPsDown(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	distributor.maxConsecutiveFails = 1
	ctx := context.Background()
	campaignID := "test-all-down"

	quotas := []ESPQuota{
		{ProfileID: "esp1", Percentage: 50},
		{ProfileID: "esp2", Percentage: 50},
	}

	// Mark both as failed
	distributor.RecordFailure(ctx, campaignID, "esp1")
	distributor.RecordFailure(ctx, campaignID, "esp2")

	// Selection should fail
	_, err := distributor.SelectESP(ctx, campaignID, quotas)
	if err != ErrNoHealthyESPs {
		t.Errorf("SelectESP() error = %v, want ErrNoHealthyESPs", err)
	}
}

func TestESPDistributor_DistributionStats(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	ctx := context.Background()
	campaignID := "test-stats"

	// Clear stats
	distributor.ClearStats(ctx, campaignID)

	// Record some sends
	for i := 0; i < 60; i++ {
		distributor.RecordSend(ctx, campaignID, "sparkpost")
	}
	for i := 0; i < 40; i++ {
		distributor.RecordSend(ctx, campaignID, "ses")
	}
	for i := 0; i < 5; i++ {
		distributor.RecordFailure(ctx, campaignID, "sparkpost")
	}

	// Get stats
	stats, err := distributor.GetDistributionStats(ctx, campaignID)
	if err != nil {
		t.Fatalf("GetDistributionStats() error: %v", err)
	}

	// Verify stats
	for _, s := range stats {
		switch s.ProfileID {
		case "sparkpost":
			if s.SentCount != 60 {
				t.Errorf("SparkPost sent = %d, want 60", s.SentCount)
			}
			if s.FailedCount != 5 {
				t.Errorf("SparkPost failed = %d, want 5", s.FailedCount)
			}
		case "ses":
			if s.SentCount != 40 {
				t.Errorf("SES sent = %d, want 40", s.SentCount)
			}
		}
	}
}

// =============================================================================
// THROTTLE MANAGER TESTS
// =============================================================================

func TestThrottleManager_SetAndGet(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	manager := NewThrottleManager(client)
	ctx := context.Background()
	campaignID := "test-throttle-campaign"

	tests := []struct {
		name         string
		rate         ThrottleRate
		customRate   int
		wantRate     int
	}{
		{"instant", ThrottleInstant, 0, 1000},
		{"gentle", ThrottleGentle, 0, 100},
		{"moderate", ThrottleModerate, 0, 50},
		{"careful", ThrottleCareful, 0, 20},
		{"custom", ThrottleCustom, 250, 250},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := manager.SetThrottle(ctx, campaignID, tt.rate, tt.customRate)
			if err != nil {
				t.Fatalf("SetThrottle() error: %v", err)
			}

			config, err := manager.GetThrottle(ctx, campaignID)
			if err != nil {
				t.Fatalf("GetThrottle() error: %v", err)
			}

			if config.RatePerMinute != tt.wantRate {
				t.Errorf("GetThrottle().RatePerMinute = %d, want %d", config.RatePerMinute, tt.wantRate)
			}
		})
	}
}

func TestThrottleManager_DefaultThrottle(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	manager := NewThrottleManager(client)
	ctx := context.Background()

	// Get throttle for non-existent campaign
	config, err := manager.GetThrottle(ctx, "non-existent")
	if err != nil {
		t.Fatalf("GetThrottle() error: %v", err)
	}

	// Should return default (gentle = 100/min)
	if config.Rate != ThrottleGentle {
		t.Errorf("Default rate = %s, want gentle", config.Rate)
	}
	if config.RatePerMinute != 100 {
		t.Errorf("Default rate/min = %d, want 100", config.RatePerMinute)
	}
}

func TestThrottleManager_RateLimitValues(t *testing.T) {
	manager := &ThrottleManager{}

	tests := []struct {
		rate       ThrottleRate
		customRate int
		want       int
	}{
		{ThrottleInstant, 0, 1000},
		{ThrottleGentle, 0, 100},
		{ThrottleModerate, 0, 50},
		{ThrottleCareful, 0, 20},
		{ThrottleCustom, 500, 500},
		{ThrottleCustom, 0, 100}, // Custom with no rate falls back to gentle
		{"invalid", 0, 100},      // Invalid falls back to gentle
	}

	for _, tt := range tests {
		t.Run(string(tt.rate), func(t *testing.T) {
			got := manager.GetRateLimit(tt.rate, tt.customRate)
			if got != tt.want {
				t.Errorf("GetRateLimit(%s, %d) = %d, want %d", tt.rate, tt.customRate, got, tt.want)
			}
		})
	}
}

// =============================================================================
// EDGE CASE TESTS
// =============================================================================

func TestESPDistributor_SmallCampaignDistribution(t *testing.T) {
	// Test case: Campaign with 10 emails, 60/40 split
	// Expected: 6 SparkPost, 4 SES
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	ctx := context.Background()
	campaignID := "small-campaign"

	quotas := []ESPQuota{
		{ProfileID: "sparkpost", Percentage: 60},
		{ProfileID: "ses", Percentage: 40},
	}

	distributor.ClearStats(ctx, campaignID)

	// Simulate 10 sends
	espCounts := make(map[string]int)
	for i := 0; i < 10; i++ {
		esp, err := distributor.SelectESP(ctx, campaignID, quotas)
		if err != nil {
			t.Fatalf("SelectESP() error: %v", err)
		}
		distributor.RecordSend(ctx, campaignID, esp)
		espCounts[esp]++
	}

	// Verify distribution is approximately 60/40
	sparkpostPct := float64(espCounts["sparkpost"]) / 10 * 100
	sesPct := float64(espCounts["ses"]) / 10 * 100

	// Allow some tolerance for small numbers
	if sparkpostPct < 50 || sparkpostPct > 70 {
		t.Errorf("SparkPost percentage = %.0f%%, want ~60%%", sparkpostPct)
	}
	if sesPct < 30 || sesPct > 50 {
		t.Errorf("SES percentage = %.0f%%, want ~40%%", sesPct)
	}

	t.Logf("Distribution: SparkPost=%d (%.0f%%), SES=%d (%.0f%%)",
		espCounts["sparkpost"], sparkpostPct, espCounts["ses"], sesPct)
}

func TestESPDistributor_RecoveryAfterFailure(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	distributor.maxConsecutiveFails = 2
	distributor.recoveryTime = 100 * time.Millisecond
	ctx := context.Background()
	campaignID := "recovery-test"

	quotas := []ESPQuota{
		{ProfileID: "primary", Percentage: 100},
	}

	// Mark primary as failed
	distributor.RecordFailure(ctx, campaignID, "primary")
	distributor.RecordFailure(ctx, campaignID, "primary")

	// Should be unhealthy
	if distributor.IsHealthy("primary") {
		t.Error("Should be unhealthy after failures")
	}

	// Should fail to select (no healthy ESPs)
	_, err := distributor.SelectESP(ctx, campaignID, quotas)
	if err != ErrNoHealthyESPs {
		t.Errorf("Expected ErrNoHealthyESPs, got: %v", err)
	}

	// Wait for recovery time
	time.Sleep(150 * time.Millisecond)

	// Should be healthy again (recovery time passed)
	if !distributor.IsHealthy("primary") {
		t.Error("Should be healthy after recovery time")
	}

	// Should be able to select now
	esp, err := distributor.SelectESP(ctx, campaignID, quotas)
	if err != nil {
		t.Errorf("SelectESP() error after recovery: %v", err)
	}
	if esp != "primary" {
		t.Errorf("SelectESP() = %s, want primary", esp)
	}
}

func TestESPDistributor_ManualHealthReset(t *testing.T) {
	client, cleanup := setupTestRedis(t)
	defer cleanup()

	distributor := NewESPDistributor(client)
	distributor.maxConsecutiveFails = 1
	ctx := context.Background()
	campaignID := "manual-reset-test"

	// Mark as failed
	distributor.RecordFailure(ctx, campaignID, "profile1")

	if distributor.IsHealthy("profile1") {
		t.Error("Should be unhealthy after failure")
	}

	// Manual reset
	distributor.ResetHealth("profile1")

	if !distributor.IsHealthy("profile1") {
		t.Error("Should be healthy after manual reset")
	}
}
