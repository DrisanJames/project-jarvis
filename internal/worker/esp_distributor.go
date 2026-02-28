package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// =============================================================================
// ESP DISTRIBUTOR - Multi-ESP Quota-Based Routing
// =============================================================================
// Distributes email sending across multiple ESPs based on configured quotas.
// Handles failover when an ESP goes down and tracks distribution statistics.

var (
	ErrNoHealthyESPs     = errors.New("no healthy ESP profiles available")
	ErrInvalidQuotas     = errors.New("ESP quotas must sum to 100%")
	ErrNoQuotasConfigured = errors.New("no ESP quotas configured")
)

// ESPQuota represents quota allocation for a single ESP
type ESPQuota struct {
	ProfileID  string `json:"profile_id"`
	Percentage int    `json:"percentage"` // 0-100
}

// ESPHealth tracks the health status of an ESP
type ESPHealth struct {
	ProfileID       string
	FailureCount    int
	LastFailure     time.Time
	ConsecutiveFails int
	IsHealthy       bool
}

// DistributionStats tracks send distribution per campaign
type DistributionStats struct {
	ProfileID   string `json:"profile_id"`
	SentCount   int64  `json:"sent_count"`
	FailedCount int64  `json:"failed_count"`
	Percentage  float64 `json:"actual_percentage"`
}

// ESPDistributor manages multi-ESP distribution with failover
type ESPDistributor struct {
	redis *redis.Client

	// Health tracking
	mu       sync.RWMutex
	health   map[string]*ESPHealth
	lastReset map[string]time.Time

	// Configuration
	maxConsecutiveFails int           // Mark unhealthy after this many consecutive failures
	healthCheckWindow   time.Duration // Time window for failure counting
	recoveryTime        time.Duration // Time before an unhealthy ESP is retried
}

// NewESPDistributor creates a new ESP distributor
func NewESPDistributor(redisClient *redis.Client) *ESPDistributor {
	return &ESPDistributor{
		redis:               redisClient,
		health:              make(map[string]*ESPHealth),
		lastReset:           make(map[string]time.Time),
		maxConsecutiveFails: 5,
		healthCheckWindow:   5 * time.Minute,
		recoveryTime:        2 * time.Minute,
	}
}

// ValidateQuotas ensures quotas sum to 100%
func ValidateQuotas(quotas []ESPQuota) error {
	if len(quotas) == 0 {
		return ErrNoQuotasConfigured
	}

	total := 0
	for _, q := range quotas {
		if q.Percentage < 0 || q.Percentage > 100 {
			return fmt.Errorf("invalid percentage %d for profile %s", q.Percentage, q.ProfileID)
		}
		total += q.Percentage
	}

	if total != 100 {
		return fmt.Errorf("%w: got %d%%", ErrInvalidQuotas, total)
	}

	return nil
}

// SelectESP chooses the next ESP based on quotas and current distribution
// Returns the profile ID to use for sending
func (d *ESPDistributor) SelectESP(ctx context.Context, campaignID string, quotas []ESPQuota) (string, error) {
	if len(quotas) == 0 {
		return "", ErrNoQuotasConfigured
	}

	// Filter to healthy ESPs only
	healthyQuotas := d.filterHealthyESPs(quotas)
	if len(healthyQuotas) == 0 {
		return "", ErrNoHealthyESPs
	}

	// If only one healthy ESP, use it
	if len(healthyQuotas) == 1 {
		return healthyQuotas[0].ProfileID, nil
	}

	// Get current distribution from Redis
	stats, err := d.GetDistributionStats(ctx, campaignID)
	if err != nil {
		// On error, fall back to first healthy ESP
		return healthyQuotas[0].ProfileID, nil
	}

	// Calculate total sent
	var totalSent int64
	for _, s := range stats {
		totalSent += s.SentCount
	}

	// Normalize quotas to healthy ESPs only
	normalizedQuotas := d.normalizeQuotas(healthyQuotas)

	// Select ESP that is furthest behind its target percentage
	var selectedESP string
	maxDeficit := -1.0

	for _, q := range normalizedQuotas {
		sentByProfile := int64(0)
		for _, s := range stats {
			if s.ProfileID == q.ProfileID {
				sentByProfile = s.SentCount
				break
			}
		}

		// Calculate expected vs actual
		expected := float64(totalSent+1) * (float64(q.Percentage) / 100.0)
		actual := float64(sentByProfile)
		deficit := expected - actual

		if deficit > maxDeficit {
			maxDeficit = deficit
			selectedESP = q.ProfileID
		}
	}

	if selectedESP == "" {
		selectedESP = healthyQuotas[0].ProfileID
	}

	return selectedESP, nil
}

// RecordSend records a successful send for distribution tracking
func (d *ESPDistributor) RecordSend(ctx context.Context, campaignID, profileID string) error {
	key := fmt.Sprintf("esp:dist:%s:%s:sent", campaignID, profileID)
	return d.redis.Incr(ctx, key).Err()
}

// RecordFailure records a send failure and updates ESP health
func (d *ESPDistributor) RecordFailure(ctx context.Context, campaignID, profileID string) error {
	// Record in distribution stats
	key := fmt.Sprintf("esp:dist:%s:%s:failed", campaignID, profileID)
	d.redis.Incr(ctx, key)

	// Update health tracking
	d.mu.Lock()
	defer d.mu.Unlock()

	h, exists := d.health[profileID]
	if !exists {
		h = &ESPHealth{
			ProfileID: profileID,
			IsHealthy: true,
		}
		d.health[profileID] = h
	}

	// Reset failure count if window expired
	if time.Since(d.lastReset[profileID]) > d.healthCheckWindow {
		h.FailureCount = 0
		h.ConsecutiveFails = 0
		d.lastReset[profileID] = time.Now()
	}

	h.FailureCount++
	h.ConsecutiveFails++
	h.LastFailure = time.Now()

	// Mark unhealthy if too many consecutive failures
	if h.ConsecutiveFails >= d.maxConsecutiveFails {
		h.IsHealthy = false
	}

	return nil
}

// RecordSuccess records a successful send and resets consecutive failure count
func (d *ESPDistributor) RecordSuccess(ctx context.Context, profileID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	h, exists := d.health[profileID]
	if !exists {
		h = &ESPHealth{
			ProfileID: profileID,
			IsHealthy: true,
		}
		d.health[profileID] = h
	}

	h.ConsecutiveFails = 0
	h.IsHealthy = true
}

// IsHealthy checks if an ESP is currently healthy
func (d *ESPDistributor) IsHealthy(profileID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	h, exists := d.health[profileID]
	if !exists {
		return true // Assume healthy if never tracked
	}

	// Check if recovery time has passed
	if !h.IsHealthy && time.Since(h.LastFailure) > d.recoveryTime {
		return true // Allow retry
	}

	return h.IsHealthy
}

// GetDistributionStats returns the current distribution statistics for a campaign
func (d *ESPDistributor) GetDistributionStats(ctx context.Context, campaignID string) ([]DistributionStats, error) {
	// Get all keys for this campaign
	pattern := fmt.Sprintf("esp:dist:%s:*", campaignID)
	keys, err := d.redis.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}

	// Aggregate stats by profile
	statsMap := make(map[string]*DistributionStats)
	var totalSent int64

	for _, key := range keys {
		val, err := d.redis.Get(ctx, key).Int64()
		if err != nil {
			continue
		}

		// Parse key: esp:dist:{campaignID}:{profileID}:{type}
		// Use strings.Split for reliable colon-delimited parsing
		parts := strings.Split(key, ":")
		if len(parts) != 5 {
			continue
		}
		profileID := parts[3]
		statType := parts[4]

		if _, exists := statsMap[profileID]; !exists {
			statsMap[profileID] = &DistributionStats{ProfileID: profileID}
		}

		switch statType {
		case "sent":
			statsMap[profileID].SentCount = val
			totalSent += val
		case "failed":
			statsMap[profileID].FailedCount = val
		}
	}

	// Calculate percentages
	result := make([]DistributionStats, 0, len(statsMap))
	for _, s := range statsMap {
		if totalSent > 0 {
			s.Percentage = float64(s.SentCount) / float64(totalSent) * 100
		}
		result = append(result, *s)
	}

	return result, nil
}

// ClearStats clears distribution statistics for a campaign
func (d *ESPDistributor) ClearStats(ctx context.Context, campaignID string) error {
	pattern := fmt.Sprintf("esp:dist:%s:*", campaignID)
	keys, err := d.redis.Keys(ctx, pattern).Result()
	if err != nil {
		return err
	}

	if len(keys) > 0 {
		return d.redis.Del(ctx, keys...).Err()
	}
	return nil
}

// filterHealthyESPs returns only healthy ESPs from the quota list
func (d *ESPDistributor) filterHealthyESPs(quotas []ESPQuota) []ESPQuota {
	healthy := make([]ESPQuota, 0, len(quotas))
	for _, q := range quotas {
		if d.IsHealthy(q.ProfileID) {
			healthy = append(healthy, q)
		}
	}
	return healthy
}

// normalizeQuotas adjusts quotas to sum to 100% after filtering
func (d *ESPDistributor) normalizeQuotas(quotas []ESPQuota) []ESPQuota {
	if len(quotas) == 0 {
		return quotas
	}

	total := 0
	for _, q := range quotas {
		total += q.Percentage
	}

	if total == 0 {
		// Equal distribution
		each := 100 / len(quotas)
		result := make([]ESPQuota, len(quotas))
		for i, q := range quotas {
			result[i] = ESPQuota{ProfileID: q.ProfileID, Percentage: each}
		}
		return result
	}

	if total == 100 {
		return quotas
	}

	// Normalize to 100%
	result := make([]ESPQuota, len(quotas))
	for i, q := range quotas {
		result[i] = ESPQuota{
			ProfileID:  q.ProfileID,
			Percentage: q.Percentage * 100 / total,
		}
	}

	return result
}

// GetHealthStatus returns the health status of all tracked ESPs
func (d *ESPDistributor) GetHealthStatus() map[string]ESPHealth {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make(map[string]ESPHealth)
	for k, v := range d.health {
		result[k] = *v
	}
	return result
}

// ResetHealth resets health status for an ESP (manual recovery)
func (d *ESPDistributor) ResetHealth(profileID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if h, exists := d.health[profileID]; exists {
		h.IsHealthy = true
		h.ConsecutiveFails = 0
		h.FailureCount = 0
	}
}

// =============================================================================
// REDIS-BASED THROTTLE MANAGER
// =============================================================================

// ThrottleManager manages campaign throttle rates in Redis for real-time updates
type ThrottleManager struct {
	redis  *redis.Client
	prefix string
}

// ThrottleRate represents a throttle speed setting
type ThrottleRate string

const (
	ThrottleInstant  ThrottleRate = "instant"  // 1000/min
	ThrottleGentle   ThrottleRate = "gentle"   // 100/min
	ThrottleModerate ThrottleRate = "moderate" // 50/min
	ThrottleCareful  ThrottleRate = "careful"  // 20/min
	ThrottleCustom   ThrottleRate = "custom"   // Custom rate
)

// ThrottleConfig holds the current throttle configuration
type ThrottleConfig struct {
	Rate          ThrottleRate `json:"rate"`
	RatePerMinute int          `json:"rate_per_minute"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

// NewThrottleManager creates a new throttle manager
func NewThrottleManager(redisClient *redis.Client) *ThrottleManager {
	return &ThrottleManager{
		redis:  redisClient,
		prefix: "campaign:throttle:",
	}
}

// SetThrottle sets the throttle rate for a campaign
func (m *ThrottleManager) SetThrottle(ctx context.Context, campaignID string, rate ThrottleRate, customRate int) error {
	config := ThrottleConfig{
		Rate:          rate,
		RatePerMinute: m.GetRateLimit(rate, customRate),
		UpdatedAt:     time.Now(),
	}

	data, err := json.Marshal(config)
	if err != nil {
		return err
	}

	// Store with 24 hour TTL
	return m.redis.Set(ctx, m.prefix+campaignID, data, 24*time.Hour).Err()
}

// GetThrottle gets the current throttle configuration for a campaign
func (m *ThrottleManager) GetThrottle(ctx context.Context, campaignID string) (*ThrottleConfig, error) {
	data, err := m.redis.Get(ctx, m.prefix+campaignID).Result()
	if err == redis.Nil {
		// Return default
		return &ThrottleConfig{
			Rate:          ThrottleGentle,
			RatePerMinute: 100,
			UpdatedAt:     time.Now(),
		}, nil
	}
	if err != nil {
		return nil, err
	}

	var config ThrottleConfig
	if err := json.Unmarshal([]byte(data), &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// GetRateLimit converts a throttle rate to requests per minute
func (m *ThrottleManager) GetRateLimit(rate ThrottleRate, customRate int) int {
	switch rate {
	case ThrottleInstant:
		return 1000
	case ThrottleGentle:
		return 100
	case ThrottleModerate:
		return 50
	case ThrottleCareful:
		return 20
	case ThrottleCustom:
		if customRate > 0 {
			return customRate
		}
		return 100 // Default to gentle if custom not specified
	default:
		return 100
	}
}

// WatchThrottle returns a channel that receives updates when throttle changes
// The caller should poll GetThrottle periodically instead of using this for simplicity
func (m *ThrottleManager) ClearThrottle(ctx context.Context, campaignID string) error {
	return m.redis.Del(ctx, m.prefix+campaignID).Err()
}
