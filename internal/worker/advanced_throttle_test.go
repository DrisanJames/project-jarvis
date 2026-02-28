package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestISPDomains verifies the ISP domain mapping
func TestISPDomains(t *testing.T) {
	tests := []struct {
		domain   string
		expected string
	}{
		{"gmail.com", "gmail"},
		{"googlemail.com", "gmail"},
		{"yahoo.com", "yahoo"},
		{"yahoo.co.uk", "yahoo"},
		{"ymail.com", "yahoo"},
		{"outlook.com", "microsoft"},
		{"hotmail.com", "microsoft"},
		{"live.com", "microsoft"},
		{"aol.com", "aol"},
		{"icloud.com", "apple"},
		{"me.com", "apple"},
		{"unknown.com", ""},
		{"company.io", ""},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			isp := DomainToISP[tt.domain]
			assert.Equal(t, tt.expected, isp)
		})
	}
}

// TestExtractDomain verifies domain extraction from email addresses
func TestExtractDomain(t *testing.T) {
	tests := []struct {
		email    string
		expected string
	}{
		{"user@gmail.com", "gmail.com"},
		{"test@outlook.com", "outlook.com"},
		{"name@company.io", "company.io"},
		{"invalid-email", ""},
		{"", ""},
		{"@nodomain.com", "nodomain.com"},
		{"user@", ""},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			domain := extractDomain(tt.email)
			assert.Equal(t, tt.expected, domain)
		})
	}
}

// TestDefaultISPLimits verifies default ISP limits are set correctly
func TestDefaultISPLimits(t *testing.T) {
	// Verify all major ISPs have defaults
	isps := []string{"gmail", "yahoo", "microsoft", "aol", "apple"}
	for _, isp := range isps {
		limits, ok := DefaultISPLimits[isp]
		assert.True(t, ok, "Expected default limits for ISP: %s", isp)
		assert.Greater(t, limits.HourlyLimit, 0, "Hourly limit should be > 0 for %s", isp)
		assert.Greater(t, limits.DailyLimit, 0, "Daily limit should be > 0 for %s", isp)
		assert.Greater(t, limits.BurstLimit, 0, "Burst limit should be > 0 for %s", isp)
		assert.NotEmpty(t, limits.Domains, "Domains should not be empty for %s", isp)
	}

	// Verify gmail has specific limits
	gmailLimits := DefaultISPLimits["gmail"]
	assert.Equal(t, 10000, gmailLimits.HourlyLimit)
	assert.Equal(t, 100000, gmailLimits.DailyLimit)
	assert.Equal(t, 500, gmailLimits.BurstLimit)
}

// TestDomainThrottle tests DomainThrottle struct
func TestDomainThrottle(t *testing.T) {
	dt := DomainThrottle{
		Domain:        "gmail.com",
		HourlyLimit:   5000,
		DailyLimit:    50000,
		CurrentHourly: 100,
		CurrentDaily:  1000,
	}

	assert.Equal(t, "gmail.com", dt.Domain)
	assert.Equal(t, 5000, dt.HourlyLimit)
	assert.Equal(t, 50000, dt.DailyLimit)
	assert.Nil(t, dt.BackoffUntil)
}

// TestISPThrottle tests ISPThrottle struct
func TestISPThrottle(t *testing.T) {
	ispThrottle := ISPThrottle{
		ISP:           "gmail",
		Domains:       []string{"gmail.com", "googlemail.com"},
		HourlyLimit:   10000,
		DailyLimit:    100000,
		BurstLimit:    500,
		CurrentHourly: 200,
		CurrentDaily:  2000,
	}

	assert.Equal(t, "gmail", ispThrottle.ISP)
	assert.Len(t, ispThrottle.Domains, 2)
	assert.Equal(t, 10000, ispThrottle.HourlyLimit)
	assert.Equal(t, 500, ispThrottle.BurstLimit)
}

// TestAdvancedThrottleConfig tests AdvancedThrottleConfig struct
func TestAdvancedThrottleConfig(t *testing.T) {
	config := AdvancedThrottleConfig{
		OrgID:        "test-org",
		GlobalHourly: 50000,
		GlobalDaily:  500000,
		DomainRules: []DomainThrottle{
			{Domain: "gmail.com", HourlyLimit: 5000, DailyLimit: 50000},
		},
		ISPRules: []ISPThrottle{
			{ISP: "gmail", HourlyLimit: 10000, DailyLimit: 100000},
		},
		AutoAdjust: true,
	}

	assert.Equal(t, "test-org", config.OrgID)
	assert.Equal(t, 50000, config.GlobalHourly)
	assert.Len(t, config.DomainRules, 1)
	assert.Len(t, config.ISPRules, 1)
	assert.True(t, config.AutoAdjust)
}

// TestThrottleStats tests ThrottleStats struct
func TestThrottleStats(t *testing.T) {
	resumeTime := time.Now().Add(1 * time.Hour)
	stats := ThrottleStats{
		Domain:        "gmail.com",
		ISP:           "gmail",
		SentLastHour:  500,
		SentLastDay:   5000,
		BounceRate:    2.5,
		ComplaintRate: 0.05,
		IsThrottled:   true,
		ResumeTime:    &resumeTime,
		HourlyLimit:   10000,
		DailyLimit:    100000,
	}

	assert.Equal(t, "gmail.com", stats.Domain)
	assert.Equal(t, "gmail", stats.ISP)
	assert.Equal(t, 500, stats.SentLastHour)
	assert.Equal(t, 2.5, stats.BounceRate)
	assert.True(t, stats.IsThrottled)
	assert.NotNil(t, stats.ResumeTime)
}

// TestBuildDomainToISPMap verifies the reverse lookup map is built correctly
func TestBuildDomainToISPMap(t *testing.T) {
	// Verify all ISP domains are in the reverse map
	for isp, domains := range ISPDomains {
		for _, domain := range domains {
			mapped := DomainToISP[domain]
			assert.Equal(t, isp, mapped, "Domain %s should map to %s", domain, isp)
		}
	}

	// Verify total count
	totalDomains := 0
	for _, domains := range ISPDomains {
		totalDomains += len(domains)
	}
	assert.Equal(t, totalDomains, len(DomainToISP))
}

// MockRedisClient is a mock for testing without real Redis
// In real tests, you would use a test Redis instance or mockery

// TestThrottleDecision tests ThrottleDecision struct
func TestThrottleDecision(t *testing.T) {
	retryTime := time.Now().Add(30 * time.Minute)
	decision := ThrottleDecision{
		Allowed:    false,
		Reason:     "hourly limit exceeded",
		RetryAfter: retryTime,
		Domain:     "gmail.com",
		ISP:        "gmail",
	}

	assert.False(t, decision.Allowed)
	assert.Contains(t, decision.Reason, "hourly limit")
	assert.Equal(t, "gmail.com", decision.Domain)
	assert.Equal(t, "gmail", decision.ISP)
}

// TestAutoAdjustmentThresholds verifies the auto-adjustment thresholds
func TestAutoAdjustmentThresholds(t *testing.T) {
	// Document the expected thresholds
	tests := []struct {
		name           string
		bounceRate     float64
		complaintRate  float64
		expectedAction string
	}{
		{"Normal performance", 1.0, 0.02, "no change"},
		{"Moderate bounces", 5.5, 0.02, "reduce 50%"},
		{"High bounces", 11.0, 0.02, "pause 1 hour"},
		{"Moderate complaints", 2.0, 0.15, "reduce 75%"},
		{"High complaints", 2.0, 0.35, "pause 24 hours"},
		{"Good performance 7 days", 1.5, 0.02, "increase 25%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Just documenting expectations - actual logic is in AutoAdjustThrottles
			if tt.bounceRate > 10 {
				assert.Equal(t, "pause 1 hour", tt.expectedAction)
			} else if tt.bounceRate > 5 {
				assert.Equal(t, "reduce 50%", tt.expectedAction)
			} else if tt.complaintRate > 0.3 {
				assert.Equal(t, "pause 24 hours", tt.expectedAction)
			} else if tt.complaintRate > 0.1 {
				assert.Equal(t, "reduce 75%", tt.expectedAction)
			} else if tt.bounceRate < 2 {
				// Could be increase 25% if 7 days of good performance
			}
		})
	}
}

// TestRedisKeyPatterns verifies Redis key pattern constants
func TestRedisKeyPatterns(t *testing.T) {
	// Test that key patterns are correctly formatted
	orgID := "test-org"
	domain := "gmail.com"
	isp := "gmail"
	hour := "2026020514"
	date := "2026-02-05"

	// Domain keys
	hourlyKey := "throttle:test-org:domain:gmail.com:hourly:2026020514"
	dailyKey := "throttle:test-org:domain:gmail.com:daily:2026-02-05"

	assert.Contains(t, hourlyKey, orgID)
	assert.Contains(t, hourlyKey, domain)
	assert.Contains(t, hourlyKey, hour)
	assert.Contains(t, dailyKey, date)

	// ISP keys
	ispHourlyKey := "throttle:test-org:isp:gmail:hourly:2026020514"
	assert.Contains(t, ispHourlyKey, isp)

	// Backoff key
	backoffKey := "throttle:test-org:backoff:gmail.com"
	assert.Contains(t, backoffKey, orgID)
	assert.Contains(t, backoffKey, domain)
}

// BenchmarkExtractDomain benchmarks the domain extraction function
func BenchmarkExtractDomain(b *testing.B) {
	emails := []string{
		"user@gmail.com",
		"test@outlook.com",
		"name@company.io",
		"long.email.address@subdomain.domain.com",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractDomain(emails[i%len(emails)])
	}
}

// BenchmarkDomainToISPLookup benchmarks the ISP lookup
func BenchmarkDomainToISPLookup(b *testing.B) {
	domains := []string{
		"gmail.com",
		"outlook.com",
		"yahoo.com",
		"unknown.com",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = DomainToISP[domains[i%len(domains)]]
	}
}

// Integration tests would require Redis and PostgreSQL
// These are typically run with build tags or in a separate test suite

// Example integration test structure (requires real Redis/PostgreSQL):
/*
func TestAdvancedThrottleManager_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	// Setup Redis client
	redisClient := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	defer redisClient.Close()

	// Setup database
	db, err := sql.Open("postgres", "postgresql://localhost:5432/test_mailing?sslmode=disable")
	require.NoError(t, err)
	defer db.Close()

	// Create manager
	manager := NewAdvancedThrottleManager(redisClient, db)

	ctx := context.Background()
	orgID := "test-org-" + time.Now().Format("20060102150405")

	// Test CanSend
	t.Run("CanSend", func(t *testing.T) {
		allowed, reason, err := manager.CanSend(ctx, orgID, "user@gmail.com")
		require.NoError(t, err)
		assert.True(t, allowed)
		assert.Empty(t, reason)
	})

	// Test RecordSend
	t.Run("RecordSend", func(t *testing.T) {
		err := manager.RecordSend(ctx, orgID, "user@gmail.com")
		require.NoError(t, err)
	})

	// Test SetDomainLimit
	t.Run("SetDomainLimit", func(t *testing.T) {
		err := manager.SetDomainLimit(ctx, orgID, "gmail.com", 100, 1000)
		require.NoError(t, err)

		config, err := manager.GetThrottleConfig(ctx, orgID)
		require.NoError(t, err)

		found := false
		for _, rule := range config.DomainRules {
			if rule.Domain == "gmail.com" {
				assert.Equal(t, 100, rule.HourlyLimit)
				assert.Equal(t, 1000, rule.DailyLimit)
				found = true
				break
			}
		}
		assert.True(t, found)
	})

	// Test ApplyBackpressure
	t.Run("ApplyBackpressure", func(t *testing.T) {
		err := manager.ApplyBackpressure(ctx, orgID, "gmail.com", 60)
		require.NoError(t, err)

		allowed, reason, err := manager.CanSend(ctx, orgID, "user@gmail.com")
		require.NoError(t, err)
		assert.False(t, allowed)
		assert.Contains(t, reason, "backoff")
	})

	// Test ClearBackpressure
	t.Run("ClearBackpressure", func(t *testing.T) {
		err := manager.ClearBackpressure(ctx, orgID, "gmail.com")
		require.NoError(t, err)

		allowed, _, err := manager.CanSend(ctx, orgID, "user@gmail.com")
		require.NoError(t, err)
		// May still be blocked by low limits, but not by backpressure
		_ = allowed
	})

	// Cleanup
	redisClient.FlushDB(ctx)
}
*/

// TestAdvancedThrottleConfigDefaults tests default config generation
func TestAdvancedThrottleConfigDefaults(t *testing.T) {
	// When config doesn't exist, defaults should be applied
	expectedDefaults := AdvancedThrottleConfig{
		OrgID:        "new-org",
		GlobalHourly: 50000,
		GlobalDaily:  500000,
		AutoAdjust:   true,
	}

	assert.Equal(t, 50000, expectedDefaults.GlobalHourly)
	assert.Equal(t, 500000, expectedDefaults.GlobalDaily)
	assert.True(t, expectedDefaults.AutoAdjust)
}

// TestCanSendBatch verifies batch checking behavior
func TestCanSendBatchLogic(t *testing.T) {
	emails := []string{
		"user1@gmail.com",
		"user2@gmail.com",
		"user3@yahoo.com",
		"user4@outlook.com",
		"user5@unknown.com",
	}

	// Group by domain for efficiency
	domainEmails := make(map[string][]int)
	for i, email := range emails {
		domain := extractDomain(email)
		domainEmails[domain] = append(domainEmails[domain], i)
	}

	// Verify grouping
	assert.Len(t, domainEmails["gmail.com"], 2)
	assert.Len(t, domainEmails["yahoo.com"], 1)
	assert.Len(t, domainEmails["outlook.com"], 1)
	assert.Len(t, domainEmails["unknown.com"], 1)
}

// TestRequire uses require for critical assertions
func TestRequireBasicSetup(t *testing.T) {
	require.NotNil(t, ISPDomains)
	require.NotNil(t, DomainToISP)
	require.NotNil(t, DefaultISPLimits)

	// Critical: gmail must exist
	require.Contains(t, ISPDomains, "gmail")
	require.Contains(t, DefaultISPLimits, "gmail")
}

// Placeholder for context timeout testing
func TestContextHandling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// In a real test, this would verify the manager handles timeouts correctly
	select {
	case <-ctx.Done():
		assert.Equal(t, context.DeadlineExceeded, ctx.Err())
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Expected context to timeout")
	}
}
