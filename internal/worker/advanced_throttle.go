package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// =============================================================================
// ADVANCED THROTTLE MANAGER - Per-Domain and Per-ISP Rate Limiting
// =============================================================================
// Provides sophisticated throttling with:
// - Per-domain hourly/daily limits
// - Per-ISP aggregate limits (gmail, yahoo, microsoft, etc.)
// - Automatic backpressure based on bounce/complaint rates
// - Auto-adjustment of limits based on delivery performance

// ISPDomains maps ISP names to their associated email domains
var ISPDomains = map[string][]string{
	"gmail":     {"gmail.com", "googlemail.com"},
	"yahoo":     {"yahoo.com", "yahoo.co.uk", "ymail.com", "rocketmail.com", "yahoo.fr", "yahoo.de", "yahoo.ca"},
	"microsoft": {"outlook.com", "hotmail.com", "live.com", "msn.com", "hotmail.co.uk", "live.co.uk"},
	"aol":       {"aol.com", "aim.com", "aol.co.uk"},
	"apple":     {"icloud.com", "me.com", "mac.com"},
	"comcast":   {"comcast.net", "xfinity.com"},
	"att":       {"att.net", "sbcglobal.net", "bellsouth.net"},
	"verizon":   {"verizon.net"},
}

// DomainToISP provides reverse lookup from domain to ISP
var DomainToISP = buildDomainToISPMap()

func buildDomainToISPMap() map[string]string {
	m := make(map[string]string)
	for isp, domains := range ISPDomains {
		for _, domain := range domains {
			m[strings.ToLower(domain)] = isp
		}
	}
	return m
}

// Default throttle limits per ISP (conservative starting values)
var DefaultISPLimits = map[string]ISPThrottle{
	"gmail": {
		ISP:         "gmail",
		Domains:     ISPDomains["gmail"],
		HourlyLimit: 10000,
		DailyLimit:  100000,
		BurstLimit:  500,
	},
	"yahoo": {
		ISP:         "yahoo",
		Domains:     ISPDomains["yahoo"],
		HourlyLimit: 8000,
		DailyLimit:  80000,
		BurstLimit:  400,
	},
	"microsoft": {
		ISP:         "microsoft",
		Domains:     ISPDomains["microsoft"],
		HourlyLimit: 10000,
		DailyLimit:  100000,
		BurstLimit:  500,
	},
	"aol": {
		ISP:         "aol",
		Domains:     ISPDomains["aol"],
		HourlyLimit: 5000,
		DailyLimit:  50000,
		BurstLimit:  250,
	},
	"apple": {
		ISP:         "apple",
		Domains:     ISPDomains["apple"],
		HourlyLimit: 8000,
		DailyLimit:  80000,
		BurstLimit:  400,
	},
}

// AdvancedThrottleManager provides per-domain and per-ISP throttling
type AdvancedThrottleManager struct {
	redis *redis.Client
	db    *sql.DB

	// Lua scripts for atomic operations
	checkAndIncrScript *redis.Script
	checkBackoffScript *redis.Script

	// Local cache for throttle configs
	configCache     map[string]*AdvancedThrottleConfig
	configCacheMu   sync.RWMutex
	configCacheTTL  time.Duration

	// Metrics tracking
	metricsCache    map[string]*throttleMetrics
	metricsCacheMu  sync.RWMutex
}

// throttleMetrics tracks recent metrics for auto-adjustment
type throttleMetrics struct {
	Bounces     int64
	Complaints  int64
	Sent        int64
	LastUpdated time.Time
}

// DomainThrottle defines throttle limits for a specific domain
type DomainThrottle struct {
	Domain        string     `json:"domain"`
	HourlyLimit   int        `json:"hourly_limit"`
	DailyLimit    int        `json:"daily_limit"`
	CurrentHourly int        `json:"current_hourly"`
	CurrentDaily  int        `json:"current_daily"`
	BackoffUntil  *time.Time `json:"backoff_until,omitempty"`
}

// ISPThrottle defines throttle limits for an ISP (aggregate across all domains)
type ISPThrottle struct {
	ISP           string   `json:"isp"`
	Domains       []string `json:"domains"`
	HourlyLimit   int      `json:"hourly_limit"`
	DailyLimit    int      `json:"daily_limit"`
	BurstLimit    int      `json:"burst_limit"` // max per minute
	CurrentHourly int      `json:"current_hourly"`
	CurrentDaily  int      `json:"current_daily"`
}

// AdvancedThrottleConfig holds complete throttle configuration for an organization
type AdvancedThrottleConfig struct {
	OrgID        string           `json:"org_id"`
	GlobalHourly int              `json:"global_hourly"`
	GlobalDaily  int              `json:"global_daily"`
	DomainRules  []DomainThrottle `json:"domain_rules"`
	ISPRules     []ISPThrottle    `json:"isp_rules"`
	AutoAdjust   bool             `json:"auto_adjust"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// ThrottleStats provides current statistics for a domain/ISP
type ThrottleStats struct {
	Domain        string     `json:"domain"`
	ISP           string     `json:"isp,omitempty"`
	SentLastHour  int        `json:"sent_last_hour"`
	SentLastDay   int        `json:"sent_last_day"`
	BounceRate    float64    `json:"bounce_rate"`
	ComplaintRate float64    `json:"complaint_rate"`
	IsThrottled   bool       `json:"is_throttled"`
	ResumeTime    *time.Time `json:"resume_time,omitempty"`
	HourlyLimit   int        `json:"hourly_limit"`
	DailyLimit    int        `json:"daily_limit"`
}

// ThrottleDecision represents the result of a throttle check
type ThrottleDecision struct {
	Allowed    bool      `json:"allowed"`
	Reason     string    `json:"reason,omitempty"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
	Domain     string    `json:"domain"`
	ISP        string    `json:"isp,omitempty"`
}

// Redis key patterns
const (
	keyDomainHourly   = "throttle:%s:domain:%s:hourly:%s"   // org_id, domain, hour
	keyDomainDaily    = "throttle:%s:domain:%s:daily:%s"    // org_id, domain, date
	keyISPHourly      = "throttle:%s:isp:%s:hourly:%s"      // org_id, isp, hour
	keyISPDaily       = "throttle:%s:isp:%s:daily:%s"       // org_id, isp, date
	keyISPBurst       = "throttle:%s:isp:%s:burst:%d"       // org_id, isp, minute
	keyBackoff        = "throttle:%s:backoff:%s"            // org_id, domain
	keyBounceCounter  = "throttle:%s:bounces:%s:%s"         // org_id, domain, hour
	keyComplaintCounter = "throttle:%s:complaints:%s:%s"    // org_id, domain, hour
	keySentCounter    = "throttle:%s:sent:%s:%s"            // org_id, domain, hour
	keyConfigCache    = "throttle:%s:config"                // org_id
)

// Lua script for atomic check-and-increment
const checkAndIncrLuaScript = `
local hourlyKey = KEYS[1]
local dailyKey = KEYS[2]
local burstKey = KEYS[3]
local increment = tonumber(ARGV[1])
local hourlyLimit = tonumber(ARGV[2])
local dailyLimit = tonumber(ARGV[3])
local burstLimit = tonumber(ARGV[4])
local hourlyTTL = tonumber(ARGV[5])
local dailyTTL = tonumber(ARGV[6])
local burstTTL = tonumber(ARGV[7])

-- Get current values
local hourlyCurrent = tonumber(redis.call("GET", hourlyKey) or "0")
local dailyCurrent = tonumber(redis.call("GET", dailyKey) or "0")
local burstCurrent = tonumber(redis.call("GET", burstKey) or "0")

-- Check limits
if hourlyCurrent + increment > hourlyLimit then
    return {0, 1, hourlyCurrent, hourlyLimit}  -- denied, reason=hourly
end
if dailyCurrent + increment > dailyLimit then
    return {0, 2, dailyCurrent, dailyLimit}  -- denied, reason=daily
end
if burstLimit > 0 and burstCurrent + increment > burstLimit then
    return {0, 3, burstCurrent, burstLimit}  -- denied, reason=burst
end

-- All checks passed - increment
local newHourly = redis.call("INCRBY", hourlyKey, increment)
if newHourly == increment then
    redis.call("EXPIRE", hourlyKey, hourlyTTL)
end

local newDaily = redis.call("INCRBY", dailyKey, increment)
if newDaily == increment then
    redis.call("EXPIRE", dailyKey, dailyTTL)
end

if burstLimit > 0 then
    local newBurst = redis.call("INCRBY", burstKey, increment)
    if newBurst == increment then
        redis.call("EXPIRE", burstKey, burstTTL)
    end
end

return {1, 0, newHourly, newDaily}  -- allowed
`

// Lua script for checking backoff
const checkBackoffLuaScript = `
local backoffKey = KEYS[1]
local backoffUntil = redis.call("GET", backoffKey)
if backoffUntil then
    local now = tonumber(ARGV[1])
    local until_ts = tonumber(backoffUntil)
    if now < until_ts then
        return {0, until_ts}  -- still in backoff
    end
end
return {1, 0}  -- not in backoff
`

// NewAdvancedThrottleManager creates a new advanced throttle manager
func NewAdvancedThrottleManager(redisClient *redis.Client, db *sql.DB) *AdvancedThrottleManager {
	return &AdvancedThrottleManager{
		redis:              redisClient,
		db:                 db,
		checkAndIncrScript: redis.NewScript(checkAndIncrLuaScript),
		checkBackoffScript: redis.NewScript(checkBackoffLuaScript),
		configCache:        make(map[string]*AdvancedThrottleConfig),
		configCacheTTL:     5 * time.Minute,
		metricsCache:       make(map[string]*throttleMetrics),
	}
}

// CanSend checks if sending to the given email is allowed under throttle limits
func (m *AdvancedThrottleManager) CanSend(ctx context.Context, orgID, email string) (bool, string, error) {
	domain := extractDomain(email)
	if domain == "" {
		return false, "invalid email format", nil
	}

	// Get throttle config for this org
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil {
		log.Printf("[AdvancedThrottle] Error getting config for org %s: %v", orgID, err)
		// Allow on error to avoid blocking all sends
		return true, "", nil
	}

	// Check domain backoff first
	backoffKey := fmt.Sprintf(keyBackoff, orgID, domain)
	backoffResult, err := m.checkBackoffScript.Run(ctx, m.redis,
		[]string{backoffKey},
		time.Now().Unix(),
	).Slice()

	if err == nil && len(backoffResult) >= 2 {
		allowed := backoffResult[0].(int64)
		if allowed == 0 {
			backoffUntil := time.Unix(backoffResult[1].(int64), 0)
			return false, fmt.Sprintf("domain %s in backoff until %s", domain, backoffUntil.Format(time.RFC3339)), nil
		}
	}

	// Check ISP-level throttle if applicable
	isp := DomainToISP[strings.ToLower(domain)]
	if isp != "" {
		allowed, reason, err := m.checkISPThrottle(ctx, orgID, isp, config)
		if err != nil {
			log.Printf("[AdvancedThrottle] ISP check error for %s: %v", isp, err)
		}
		if !allowed {
			return false, reason, nil
		}
	}

	// Check domain-level throttle
	allowed, reason, err := m.checkDomainThrottle(ctx, orgID, domain, config)
	if err != nil {
		log.Printf("[AdvancedThrottle] Domain check error for %s: %v", domain, err)
	}
	if !allowed {
		return false, reason, nil
	}

	// Check global org limits
	allowed, reason, err = m.checkGlobalThrottle(ctx, orgID, config)
	if err != nil {
		log.Printf("[AdvancedThrottle] Global check error: %v", err)
	}
	if !allowed {
		return false, reason, nil
	}

	return true, "", nil
}

// checkISPThrottle checks ISP-level throttle limits
func (m *AdvancedThrottleManager) checkISPThrottle(ctx context.Context, orgID, isp string, config *AdvancedThrottleConfig) (bool, string, error) {
	// Find ISP rule in config
	var ispRule *ISPThrottle
	for i := range config.ISPRules {
		if config.ISPRules[i].ISP == isp {
			ispRule = &config.ISPRules[i]
			break
		}
	}

	// Fall back to defaults if no custom rule
	if ispRule == nil {
		if defaultRule, ok := DefaultISPLimits[isp]; ok {
			ispRule = &defaultRule
		} else {
			// No limits for this ISP
			return true, "", nil
		}
	}

	now := time.Now()
	hourKey := fmt.Sprintf(keyISPHourly, orgID, isp, now.Format("2006010215"))
	dayKey := fmt.Sprintf(keyISPDaily, orgID, isp, now.Format("2006-01-02"))
	burstKey := fmt.Sprintf(keyISPBurst, orgID, isp, now.Unix()/60)

	result, err := m.checkAndIncrScript.Run(ctx, m.redis,
		[]string{hourKey, dayKey, burstKey},
		1,
		ispRule.HourlyLimit,
		ispRule.DailyLimit,
		ispRule.BurstLimit,
		3700,  // hourly TTL (1 hour + buffer)
		90000, // daily TTL (25 hours)
		120,   // burst TTL (2 minutes)
	).Slice()

	if err != nil {
		return true, "", err // Allow on error
	}

	allowed := result[0].(int64) == 1
	if !allowed {
		reason := result[1].(int64)
		current := result[2].(int64)
		limit := result[3].(int64)

		switch reason {
		case 1:
			return false, fmt.Sprintf("ISP %s hourly limit reached (%d/%d)", isp, current, limit), nil
		case 2:
			return false, fmt.Sprintf("ISP %s daily limit reached (%d/%d)", isp, current, limit), nil
		case 3:
			return false, fmt.Sprintf("ISP %s burst limit reached (%d/%d)", isp, current, limit), nil
		}
	}

	return true, "", nil
}

// checkDomainThrottle checks domain-level throttle limits
func (m *AdvancedThrottleManager) checkDomainThrottle(ctx context.Context, orgID, domain string, config *AdvancedThrottleConfig) (bool, string, error) {
	// Find domain rule in config
	var domainRule *DomainThrottle
	for i := range config.DomainRules {
		if strings.EqualFold(config.DomainRules[i].Domain, domain) {
			domainRule = &config.DomainRules[i]
			break
		}
	}

	// If no specific domain rule, use defaults based on ISP or general defaults
	if domainRule == nil {
		domainRule = &DomainThrottle{
			Domain:      domain,
			HourlyLimit: 5000,  // Default domain limit
			DailyLimit:  50000, // Default domain limit
		}
	}

	now := time.Now()
	hourKey := fmt.Sprintf(keyDomainHourly, orgID, domain, now.Format("2006010215"))
	dayKey := fmt.Sprintf(keyDomainDaily, orgID, domain, now.Format("2006-01-02"))

	result, err := m.checkAndIncrScript.Run(ctx, m.redis,
		[]string{hourKey, dayKey, ""},
		1,
		domainRule.HourlyLimit,
		domainRule.DailyLimit,
		0,     // No burst limit for domains
		3700,  // hourly TTL
		90000, // daily TTL
		0,     // burst TTL (unused)
	).Slice()

	if err != nil {
		return true, "", err // Allow on error
	}

	allowed := result[0].(int64) == 1
	if !allowed {
		reason := result[1].(int64)
		current := result[2].(int64)
		limit := result[3].(int64)

		switch reason {
		case 1:
			return false, fmt.Sprintf("domain %s hourly limit reached (%d/%d)", domain, current, limit), nil
		case 2:
			return false, fmt.Sprintf("domain %s daily limit reached (%d/%d)", domain, current, limit), nil
		}
	}

	return true, "", nil
}

// checkGlobalThrottle checks organization-level global throttle limits
func (m *AdvancedThrottleManager) checkGlobalThrottle(ctx context.Context, orgID string, config *AdvancedThrottleConfig) (bool, string, error) {
	if config.GlobalHourly == 0 && config.GlobalDaily == 0 {
		return true, "", nil // No global limits set
	}

	now := time.Now()
	hourKey := fmt.Sprintf("throttle:%s:global:hourly:%s", orgID, now.Format("2006010215"))
	dayKey := fmt.Sprintf("throttle:%s:global:daily:%s", orgID, now.Format("2006-01-02"))

	result, err := m.checkAndIncrScript.Run(ctx, m.redis,
		[]string{hourKey, dayKey, ""},
		1,
		config.GlobalHourly,
		config.GlobalDaily,
		0,     // No burst limit
		3700,  // hourly TTL
		90000, // daily TTL
		0,     // burst TTL (unused)
	).Slice()

	if err != nil {
		return true, "", err // Allow on error
	}

	allowed := result[0].(int64) == 1
	if !allowed {
		reason := result[1].(int64)
		current := result[2].(int64)
		limit := result[3].(int64)

		switch reason {
		case 1:
			return false, fmt.Sprintf("global hourly limit reached (%d/%d)", current, limit), nil
		case 2:
			return false, fmt.Sprintf("global daily limit reached (%d/%d)", current, limit), nil
		}
	}

	return true, "", nil
}

// RecordSend records a successful send for metrics tracking
func (m *AdvancedThrottleManager) RecordSend(ctx context.Context, orgID, email string) error {
	domain := extractDomain(email)
	if domain == "" {
		return nil
	}

	now := time.Now()
	key := fmt.Sprintf(keySentCounter, orgID, domain, now.Format("2006010215"))

	pipe := m.redis.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 25*time.Hour)
	_, err := pipe.Exec(ctx)

	return err
}

// RecordBounce records a bounce and may trigger backpressure
func (m *AdvancedThrottleManager) RecordBounce(ctx context.Context, orgID, email, bounceType string) error {
	domain := extractDomain(email)
	if domain == "" {
		return nil
	}

	now := time.Now()
	bounceKey := fmt.Sprintf(keyBounceCounter, orgID, domain, now.Format("2006010215"))
	sentKey := fmt.Sprintf(keySentCounter, orgID, domain, now.Format("2006010215"))

	pipe := m.redis.Pipeline()
	bounceCmd := pipe.Incr(ctx, bounceKey)
	pipe.Expire(ctx, bounceKey, 25*time.Hour)
	sentCmd := pipe.Get(ctx, sentKey)
	_, err := pipe.Exec(ctx)

	if err != nil && err != redis.Nil {
		return err
	}

	// Check if we need to trigger auto-adjustment
	bounces := bounceCmd.Val()
	sent, _ := sentCmd.Int64()
	if sent == 0 {
		sent = 1
	}

	bounceRate := float64(bounces) / float64(sent) * 100

	// Get config to check if auto-adjust is enabled
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil || !config.AutoAdjust {
		return nil
	}

	// Apply backpressure based on bounce rate
	if bounceRate > 10 {
		// Pause domain for 1 hour
		log.Printf("[AdvancedThrottle] Bounce rate %.2f%% for %s - pausing for 1 hour", bounceRate, domain)
		return m.ApplyBackpressure(ctx, orgID, domain, 3600)
	} else if bounceRate > 5 {
		// Reduce limits by 50%
		log.Printf("[AdvancedThrottle] Bounce rate %.2f%% for %s - reducing limits by 50%%", bounceRate, domain)
		return m.reduceDomainLimits(ctx, orgID, domain, 0.5)
	}

	return nil
}

// RecordComplaint records a complaint and may trigger backpressure
func (m *AdvancedThrottleManager) RecordComplaint(ctx context.Context, orgID, email string) error {
	domain := extractDomain(email)
	if domain == "" {
		return nil
	}

	now := time.Now()
	complaintKey := fmt.Sprintf(keyComplaintCounter, orgID, domain, now.Format("2006010215"))
	sentKey := fmt.Sprintf(keySentCounter, orgID, domain, now.Format("2006010215"))

	pipe := m.redis.Pipeline()
	complaintCmd := pipe.Incr(ctx, complaintKey)
	pipe.Expire(ctx, complaintKey, 25*time.Hour)
	sentCmd := pipe.Get(ctx, sentKey)
	_, err := pipe.Exec(ctx)

	if err != nil && err != redis.Nil {
		return err
	}

	complaints := complaintCmd.Val()
	sent, _ := sentCmd.Int64()
	if sent == 0 {
		sent = 1
	}

	complaintRate := float64(complaints) / float64(sent) * 100

	// Get config to check if auto-adjust is enabled
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil || !config.AutoAdjust {
		return nil
	}

	// Apply backpressure based on complaint rate
	if complaintRate > 0.3 {
		// Pause domain for 24 hours
		log.Printf("[AdvancedThrottle] Complaint rate %.3f%% for %s - pausing for 24 hours", complaintRate, domain)
		return m.ApplyBackpressure(ctx, orgID, domain, 86400)
	} else if complaintRate > 0.1 {
		// Reduce limits by 75%
		log.Printf("[AdvancedThrottle] Complaint rate %.3f%% for %s - reducing limits by 75%%", complaintRate, domain)
		return m.reduceDomainLimits(ctx, orgID, domain, 0.25)
	}

	return nil
}

// SetDomainLimit sets throttle limits for a specific domain
func (m *AdvancedThrottleManager) SetDomainLimit(ctx context.Context, orgID, domain string, hourly, daily int) error {
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil {
		config = &AdvancedThrottleConfig{
			OrgID:       orgID,
			DomainRules: []DomainThrottle{},
			ISPRules:    []ISPThrottle{},
		}
	}

	// Update or add domain rule
	found := false
	for i := range config.DomainRules {
		if strings.EqualFold(config.DomainRules[i].Domain, domain) {
			config.DomainRules[i].HourlyLimit = hourly
			config.DomainRules[i].DailyLimit = daily
			found = true
			break
		}
	}

	if !found {
		config.DomainRules = append(config.DomainRules, DomainThrottle{
			Domain:      domain,
			HourlyLimit: hourly,
			DailyLimit:  daily,
		})
	}

	return m.SetThrottleConfig(ctx, *config)
}

// SetISPLimit sets throttle limits for an ISP
func (m *AdvancedThrottleManager) SetISPLimit(ctx context.Context, orgID, isp string, hourly, daily, burst int) error {
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil {
		config = &AdvancedThrottleConfig{
			OrgID:       orgID,
			DomainRules: []DomainThrottle{},
			ISPRules:    []ISPThrottle{},
		}
	}

	// Get ISP domains
	domains, ok := ISPDomains[isp]
	if !ok {
		return fmt.Errorf("unknown ISP: %s", isp)
	}

	// Update or add ISP rule
	found := false
	for i := range config.ISPRules {
		if config.ISPRules[i].ISP == isp {
			config.ISPRules[i].HourlyLimit = hourly
			config.ISPRules[i].DailyLimit = daily
			config.ISPRules[i].BurstLimit = burst
			found = true
			break
		}
	}

	if !found {
		config.ISPRules = append(config.ISPRules, ISPThrottle{
			ISP:         isp,
			Domains:     domains,
			HourlyLimit: hourly,
			DailyLimit:  daily,
			BurstLimit:  burst,
		})
	}

	return m.SetThrottleConfig(ctx, *config)
}

// GetDomainStats returns current throttle statistics for all domains
func (m *AdvancedThrottleManager) GetDomainStats(ctx context.Context, orgID string) ([]ThrottleStats, error) {
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil {
		return nil, err
	}

	var stats []ThrottleStats
	now := time.Now()

	// Get stats for each domain in the config
	domainsToCheck := make(map[string]bool)
	for _, rule := range config.DomainRules {
		domainsToCheck[rule.Domain] = true
	}

	// Also check all ISP domains
	for _, ispRule := range config.ISPRules {
		for _, domain := range ispRule.Domains {
			domainsToCheck[domain] = true
		}
	}

	// Add common ISP domains if not already present
	for _, domains := range ISPDomains {
		for _, domain := range domains {
			domainsToCheck[domain] = true
		}
	}

	for domain := range domainsToCheck {
		stat, err := m.getDomainStat(ctx, orgID, domain, now, config)
		if err != nil {
			log.Printf("[AdvancedThrottle] Error getting stats for %s: %v", domain, err)
			continue
		}
		stats = append(stats, *stat)
	}

	return stats, nil
}

// getDomainStat retrieves statistics for a single domain
func (m *AdvancedThrottleManager) getDomainStat(ctx context.Context, orgID, domain string, now time.Time, config *AdvancedThrottleConfig) (*ThrottleStats, error) {
	hourKey := fmt.Sprintf(keyDomainHourly, orgID, domain, now.Format("2006010215"))
	dayKey := fmt.Sprintf(keyDomainDaily, orgID, domain, now.Format("2006-01-02"))
	bounceKey := fmt.Sprintf(keyBounceCounter, orgID, domain, now.Format("2006010215"))
	complaintKey := fmt.Sprintf(keyComplaintCounter, orgID, domain, now.Format("2006010215"))
	sentKey := fmt.Sprintf(keySentCounter, orgID, domain, now.Format("2006010215"))
	backoffKey := fmt.Sprintf(keyBackoff, orgID, domain)

	pipe := m.redis.Pipeline()
	hourlyCmd := pipe.Get(ctx, hourKey)
	dailyCmd := pipe.Get(ctx, dayKey)
	bounceCmd := pipe.Get(ctx, bounceKey)
	complaintCmd := pipe.Get(ctx, complaintKey)
	sentCmd := pipe.Get(ctx, sentKey)
	backoffCmd := pipe.Get(ctx, backoffKey)
	pipe.Exec(ctx)

	hourly, _ := hourlyCmd.Int64()
	daily, _ := dailyCmd.Int64()
	bounces, _ := bounceCmd.Int64()
	complaints, _ := complaintCmd.Int64()
	sent, _ := sentCmd.Int64()
	backoffStr, _ := backoffCmd.Result()

	if sent == 0 {
		sent = 1 // Avoid division by zero
	}

	stat := &ThrottleStats{
		Domain:        domain,
		ISP:           DomainToISP[strings.ToLower(domain)],
		SentLastHour:  int(hourly),
		SentLastDay:   int(daily),
		BounceRate:    float64(bounces) / float64(sent) * 100,
		ComplaintRate: float64(complaints) / float64(sent) * 100,
		IsThrottled:   false,
	}

	// Find domain limits
	for _, rule := range config.DomainRules {
		if strings.EqualFold(rule.Domain, domain) {
			stat.HourlyLimit = rule.HourlyLimit
			stat.DailyLimit = rule.DailyLimit
			break
		}
	}

	// Use defaults if not found
	if stat.HourlyLimit == 0 {
		stat.HourlyLimit = 5000
		stat.DailyLimit = 50000
	}

	// Check if throttled
	if int(hourly) >= stat.HourlyLimit || int(daily) >= stat.DailyLimit {
		stat.IsThrottled = true
	}

	// Check backoff
	if backoffStr != "" {
		backoffTs, err := time.Parse(time.RFC3339, backoffStr)
		if err == nil && backoffTs.After(now) {
			stat.IsThrottled = true
			stat.ResumeTime = &backoffTs
		}
	}

	return stat, nil
}

// ApplyBackpressure applies a temporary pause to sending for a domain
func (m *AdvancedThrottleManager) ApplyBackpressure(ctx context.Context, orgID, domain string, seconds int) error {
	backoffUntil := time.Now().Add(time.Duration(seconds) * time.Second)
	key := fmt.Sprintf(keyBackoff, orgID, domain)

	err := m.redis.Set(ctx, key, backoffUntil.Unix(), time.Duration(seconds)*time.Second).Err()
	if err != nil {
		return fmt.Errorf("failed to set backpressure: %w", err)
	}

	// Also record in database for persistence
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO mailing_throttle_backpressure (org_id, domain, backoff_until, reason, created_at)
		VALUES ($1, $2, $3, 'auto-applied', NOW())
		ON CONFLICT (org_id, domain) DO UPDATE SET
			backoff_until = $3,
			updated_at = NOW()
	`, orgID, domain, backoffUntil)

	if err != nil {
		log.Printf("[AdvancedThrottle] Failed to persist backpressure to DB: %v", err)
	}

	log.Printf("[AdvancedThrottle] Applied backpressure for %s:%s until %s", orgID, domain, backoffUntil.Format(time.RFC3339))
	return nil
}

// ClearBackpressure removes backpressure for a domain
func (m *AdvancedThrottleManager) ClearBackpressure(ctx context.Context, orgID, domain string) error {
	key := fmt.Sprintf(keyBackoff, orgID, domain)

	err := m.redis.Del(ctx, key).Err()
	if err != nil {
		return fmt.Errorf("failed to clear backpressure: %w", err)
	}

	// Also remove from database
	_, err = m.db.ExecContext(ctx, `
		DELETE FROM mailing_throttle_backpressure
		WHERE org_id = $1 AND domain = $2
	`, orgID, domain)

	log.Printf("[AdvancedThrottle] Cleared backpressure for %s:%s", orgID, domain)
	return nil
}

// AutoAdjustThrottles analyzes performance and adjusts limits automatically
func (m *AdvancedThrottleManager) AutoAdjustThrottles(ctx context.Context, orgID string) error {
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil {
		return err
	}

	if !config.AutoAdjust {
		return nil
	}

	// Get 7-day bounce/complaint stats from database
	rows, err := m.db.QueryContext(ctx, `
		SELECT 
			domain,
			SUM(sent_count) as sent,
			SUM(bounce_count) as bounces,
			SUM(complaint_count) as complaints,
			COUNT(DISTINCT DATE(recorded_at)) as days
		FROM mailing_throttle_daily_stats
		WHERE org_id = $1
		  AND recorded_at >= NOW() - INTERVAL '7 days'
		GROUP BY domain
	`, orgID)

	if err != nil {
		return fmt.Errorf("failed to query stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var domain string
		var sent, bounces, complaints, days int64

		if err := rows.Scan(&domain, &sent, &bounces, &complaints, &days); err != nil {
			continue
		}

		if sent == 0 || days < 3 {
			continue // Not enough data
		}

		bounceRate := float64(bounces) / float64(sent) * 100
		complaintRate := float64(complaints) / float64(sent) * 100

		// Find current limit
		var currentLimit int
		for _, rule := range config.DomainRules {
			if strings.EqualFold(rule.Domain, domain) {
				currentLimit = rule.HourlyLimit
				break
			}
		}
		if currentLimit == 0 {
			currentLimit = 5000 // Default
		}

		// Adjust based on performance
		var newLimit int
		if complaintRate > 0.3 {
			// Pause completely (set very low limit)
			newLimit = 100
			log.Printf("[AutoAdjust] %s: complaint rate %.3f%% - reducing to %d/hour", domain, complaintRate, newLimit)
		} else if complaintRate > 0.1 {
			// Reduce by 75%
			newLimit = int(float64(currentLimit) * 0.25)
			log.Printf("[AutoAdjust] %s: complaint rate %.3f%% - reducing to %d/hour", domain, complaintRate, newLimit)
		} else if bounceRate > 10 {
			// Reduce by 75%
			newLimit = int(float64(currentLimit) * 0.25)
			log.Printf("[AutoAdjust] %s: bounce rate %.2f%% - reducing to %d/hour", domain, bounceRate, newLimit)
		} else if bounceRate > 5 {
			// Reduce by 50%
			newLimit = int(float64(currentLimit) * 0.5)
			log.Printf("[AutoAdjust] %s: bounce rate %.2f%% - reducing to %d/hour", domain, bounceRate, newLimit)
		} else if bounceRate < 2 && days >= 7 {
			// Good performance for 7 days - increase by 25%
			newLimit = int(float64(currentLimit) * 1.25)
			log.Printf("[AutoAdjust] %s: good performance (%.2f%% bounce) - increasing to %d/hour", domain, bounceRate, newLimit)
		}

		if newLimit > 0 && newLimit != currentLimit {
			// Apply new limit
			if err := m.SetDomainLimit(ctx, orgID, domain, newLimit, newLimit*10); err != nil {
				log.Printf("[AutoAdjust] Failed to update limit for %s: %v", domain, err)
			}
		}
	}

	return nil
}

// GetThrottleConfig retrieves the throttle configuration for an organization
func (m *AdvancedThrottleManager) GetThrottleConfig(ctx context.Context, orgID string) (*AdvancedThrottleConfig, error) {
	// Check local cache first
	m.configCacheMu.RLock()
	if cached, ok := m.configCache[orgID]; ok {
		if time.Since(cached.UpdatedAt) < m.configCacheTTL {
			m.configCacheMu.RUnlock()
			return cached, nil
		}
	}
	m.configCacheMu.RUnlock()

	// Try Redis cache
	cacheKey := fmt.Sprintf(keyConfigCache, orgID)
	data, err := m.redis.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var config AdvancedThrottleConfig
		if json.Unmarshal(data, &config) == nil {
			m.configCacheMu.Lock()
			m.configCache[orgID] = &config
			m.configCacheMu.Unlock()
			return &config, nil
		}
	}

	// Load from database
	var config AdvancedThrottleConfig
	var configJSON []byte

	err = m.db.QueryRowContext(ctx, `
		SELECT org_id, global_hourly_limit, global_daily_limit, 
		       domain_rules, isp_rules, auto_adjust, updated_at
		FROM mailing_throttle_configs
		WHERE org_id = $1
	`, orgID).Scan(
		&config.OrgID,
		&config.GlobalHourly,
		&config.GlobalDaily,
		&configJSON,
		&configJSON, // Will parse separately
		&config.AutoAdjust,
		&config.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		// Return default config
		config = AdvancedThrottleConfig{
			OrgID:        orgID,
			GlobalHourly: 50000,
			GlobalDaily:  500000,
			DomainRules:  []DomainThrottle{},
			ISPRules:     []ISPThrottle{},
			AutoAdjust:   true,
			UpdatedAt:    time.Now(),
		}

		// Initialize with default ISP rules
		for isp, limits := range DefaultISPLimits {
			config.ISPRules = append(config.ISPRules, limits)
			_ = isp
		}

		return &config, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Parse domain and ISP rules
	var domainRulesJSON, ispRulesJSON []byte
	m.db.QueryRowContext(ctx, `
		SELECT domain_rules, isp_rules
		FROM mailing_throttle_configs
		WHERE org_id = $1
	`, orgID).Scan(&domainRulesJSON, &ispRulesJSON)

	if len(domainRulesJSON) > 0 {
		json.Unmarshal(domainRulesJSON, &config.DomainRules)
	}
	if len(ispRulesJSON) > 0 {
		json.Unmarshal(ispRulesJSON, &config.ISPRules)
	}

	// Cache the result
	configData, _ := json.Marshal(&config)
	m.redis.Set(ctx, cacheKey, configData, 5*time.Minute)

	m.configCacheMu.Lock()
	m.configCache[orgID] = &config
	m.configCacheMu.Unlock()

	return &config, nil
}

// SetThrottleConfig saves the throttle configuration for an organization
func (m *AdvancedThrottleManager) SetThrottleConfig(ctx context.Context, config AdvancedThrottleConfig) error {
	domainRulesJSON, err := json.Marshal(config.DomainRules)
	if err != nil {
		return fmt.Errorf("failed to marshal domain rules: %w", err)
	}

	ispRulesJSON, err := json.Marshal(config.ISPRules)
	if err != nil {
		return fmt.Errorf("failed to marshal ISP rules: %w", err)
	}

	_, err = m.db.ExecContext(ctx, `
		INSERT INTO mailing_throttle_configs (org_id, global_hourly_limit, global_daily_limit, domain_rules, isp_rules, auto_adjust, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (org_id) DO UPDATE SET
			global_hourly_limit = $2,
			global_daily_limit = $3,
			domain_rules = $4,
			isp_rules = $5,
			auto_adjust = $6,
			updated_at = NOW()
	`, config.OrgID, config.GlobalHourly, config.GlobalDaily, domainRulesJSON, ispRulesJSON, config.AutoAdjust)

	if err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	// Invalidate caches
	cacheKey := fmt.Sprintf(keyConfigCache, config.OrgID)
	m.redis.Del(ctx, cacheKey)

	m.configCacheMu.Lock()
	delete(m.configCache, config.OrgID)
	m.configCacheMu.Unlock()

	return nil
}

// reduceDomainLimits reduces limits for a domain by a factor
func (m *AdvancedThrottleManager) reduceDomainLimits(ctx context.Context, orgID, domain string, factor float64) error {
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil {
		return err
	}

	// Find and update domain rule
	for i := range config.DomainRules {
		if strings.EqualFold(config.DomainRules[i].Domain, domain) {
			config.DomainRules[i].HourlyLimit = int(float64(config.DomainRules[i].HourlyLimit) * factor)
			config.DomainRules[i].DailyLimit = int(float64(config.DomainRules[i].DailyLimit) * factor)
			return m.SetThrottleConfig(ctx, *config)
		}
	}

	// Domain not in config, add with reduced defaults
	config.DomainRules = append(config.DomainRules, DomainThrottle{
		Domain:      domain,
		HourlyLimit: int(5000 * factor),
		DailyLimit:  int(50000 * factor),
	})

	return m.SetThrottleConfig(ctx, *config)
}

// extractDomain extracts the domain from an email address
func extractDomain(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return ""
	}
	return strings.ToLower(parts[1])
}

// CanSendBatch checks if a batch of emails can be sent (for batch processing)
func (m *AdvancedThrottleManager) CanSendBatch(ctx context.Context, orgID string, emails []string) ([]ThrottleDecision, error) {
	decisions := make([]ThrottleDecision, len(emails))

	// Group by domain for efficiency
	domainEmails := make(map[string][]int)
	for i, email := range emails {
		domain := extractDomain(email)
		domainEmails[domain] = append(domainEmails[domain], i)
	}

	// Check each domain
	for domain, indices := range domainEmails {
		for _, idx := range indices {
			allowed, reason, err := m.CanSend(ctx, orgID, emails[idx])
			if err != nil {
				log.Printf("[AdvancedThrottle] Error checking %s: %v", logger.RedactEmail(emails[idx]), err)
				allowed = true // Allow on error
			}

			decisions[idx] = ThrottleDecision{
				Allowed: allowed,
				Reason:  reason,
				Domain:  domain,
				ISP:     DomainToISP[domain],
			}
		}
	}

	return decisions, nil
}

// RecordDailyStats persists daily statistics to the database for long-term analysis
func (m *AdvancedThrottleManager) RecordDailyStats(ctx context.Context, orgID string) error {
	now := time.Now()
	dateStr := now.Format("2006-01-02")
	hourStr := now.Add(-1 * time.Hour).Format("2006010215") // Previous hour

	// Get all domains that had activity in the past hour
	pattern := fmt.Sprintf("throttle:%s:domain:*:hourly:%s", orgID, hourStr)
	keys, err := m.redis.Keys(ctx, pattern).Result()
	if err != nil {
		return err
	}

	for _, key := range keys {
		// Extract domain from key
		parts := strings.Split(key, ":")
		if len(parts) < 4 {
			continue
		}
		domain := parts[3]

		// Get stats
		hourlyKey := fmt.Sprintf(keyDomainHourly, orgID, domain, hourStr)
		bounceKey := fmt.Sprintf(keyBounceCounter, orgID, domain, hourStr)
		complaintKey := fmt.Sprintf(keyComplaintCounter, orgID, domain, hourStr)

		pipe := m.redis.Pipeline()
		sentCmd := pipe.Get(ctx, hourlyKey)
		bounceCmd := pipe.Get(ctx, bounceKey)
		complaintCmd := pipe.Get(ctx, complaintKey)
		pipe.Exec(ctx)

		sent, _ := sentCmd.Int64()
		bounces, _ := bounceCmd.Int64()
		complaints, _ := complaintCmd.Int64()

		if sent == 0 {
			continue
		}

		// Persist to database
		_, err := m.db.ExecContext(ctx, `
			INSERT INTO mailing_throttle_daily_stats (org_id, domain, recorded_at, sent_count, bounce_count, complaint_count)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (org_id, domain, recorded_at) DO UPDATE SET
				sent_count = mailing_throttle_daily_stats.sent_count + $4,
				bounce_count = mailing_throttle_daily_stats.bounce_count + $5,
				complaint_count = mailing_throttle_daily_stats.complaint_count + $6
		`, orgID, domain, dateStr, sent, bounces, complaints)

		if err != nil {
			log.Printf("[AdvancedThrottle] Failed to persist daily stats for %s: %v", domain, err)
		}
	}

	return nil
}

// GetISPStats returns current throttle statistics for all ISPs
func (m *AdvancedThrottleManager) GetISPStats(ctx context.Context, orgID string) ([]ThrottleStats, error) {
	config, err := m.GetThrottleConfig(ctx, orgID)
	if err != nil {
		return nil, err
	}

	var stats []ThrottleStats
	now := time.Now()

	for isp := range ISPDomains {
		stat, err := m.getISPStat(ctx, orgID, isp, now, config)
		if err != nil {
			log.Printf("[AdvancedThrottle] Error getting ISP stats for %s: %v", isp, err)
			continue
		}
		stats = append(stats, *stat)
	}

	return stats, nil
}

// getISPStat retrieves statistics for a single ISP
func (m *AdvancedThrottleManager) getISPStat(ctx context.Context, orgID, isp string, now time.Time, config *AdvancedThrottleConfig) (*ThrottleStats, error) {
	hourKey := fmt.Sprintf(keyISPHourly, orgID, isp, now.Format("2006010215"))
	dayKey := fmt.Sprintf(keyISPDaily, orgID, isp, now.Format("2006-01-02"))

	pipe := m.redis.Pipeline()
	hourlyCmd := pipe.Get(ctx, hourKey)
	dailyCmd := pipe.Get(ctx, dayKey)
	pipe.Exec(ctx)

	hourly, _ := hourlyCmd.Int64()
	daily, _ := dailyCmd.Int64()

	stat := &ThrottleStats{
		ISP:          isp,
		SentLastHour: int(hourly),
		SentLastDay:  int(daily),
		IsThrottled:  false,
	}

	// Find ISP limits
	for _, rule := range config.ISPRules {
		if rule.ISP == isp {
			stat.HourlyLimit = rule.HourlyLimit
			stat.DailyLimit = rule.DailyLimit
			break
		}
	}

	// Use defaults if not found
	if stat.HourlyLimit == 0 {
		if defaultLimits, ok := DefaultISPLimits[isp]; ok {
			stat.HourlyLimit = defaultLimits.HourlyLimit
			stat.DailyLimit = defaultLimits.DailyLimit
		}
	}

	// Check if throttled
	if int(hourly) >= stat.HourlyLimit || int(daily) >= stat.DailyLimit {
		stat.IsThrottled = true
	}

	return stat, nil
}

// Close closes the throttle manager
func (m *AdvancedThrottleManager) Close() error {
	return nil
}
