package engine

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ISPRateRegistry is a thread-safe registry of per-ISP rate limiters.
// ThrottleAgents call SetRate when they compute new rates.
// The dispatch loop calls AllowN before claiming a batch for each ISP.
type ISPRateRegistry struct {
	mu       sync.RWMutex
	limiters map[ISP]*rate.Limiter
	rates    map[ISP]float64 // current msgs/hour for logging
}

// NewISPRateRegistry creates an empty registry. Call SetRate for each ISP
// to populate it with initial rates before use.
func NewISPRateRegistry() *ISPRateRegistry {
	return &ISPRateRegistry{
		limiters: make(map[ISP]*rate.Limiter),
		rates:    make(map[ISP]float64),
	}
}

// SetRate sets or updates the rate limit for an ISP.
// msgsPerHour is the sustained sending rate (e.g., 500 means 500 messages/hour).
// The burst is set to ~10 seconds of budget (msgsPerHour/360), minimum 1.
func (r *ISPRateRegistry) SetRate(isp ISP, msgsPerHour float64) {
	perSecond := msgsPerHour / 3600.0
	burst := int(math.Max(1, msgsPerHour/360.0))

	r.mu.Lock()
	defer r.mu.Unlock()

	if lim, ok := r.limiters[isp]; ok {
		lim.SetLimit(rate.Limit(perSecond))
		lim.SetBurst(burst)
	} else {
		r.limiters[isp] = rate.NewLimiter(rate.Limit(perSecond), burst)
	}
	r.rates[isp] = msgsPerHour
}

// AllowN checks if the ISP's rate limiter can permit 'requested' messages.
// Returns how many of the requested messages are allowed (0 to requested).
// Unknown ISPs (not in the registry) are always fully permitted.
func (r *ISPRateRegistry) AllowN(isp string, requested int) int {
	if requested <= 0 {
		return 0
	}

	r.mu.RLock()
	lim, ok := r.limiters[ISP(isp)]
	r.mu.RUnlock()

	if !ok {
		return requested
	}

	now := time.Now()
	available := int(lim.TokensAt(now))
	if available <= 0 {
		return 0
	}

	n := requested
	if n > available {
		n = available
	}

	if lim.AllowN(now, n) {
		return n
	}
	return 0
}

// GetRate returns the current rate in msgs/hour for an ISP.
// Returns 0 if the ISP is not in the registry.
func (r *ISPRateRegistry) GetRate(isp ISP) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rates[isp]
}

// GetAllRates returns a snapshot of all current ISP rates in msgs/hour.
func (r *ISPRateRegistry) GetAllRates() map[ISP]float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[ISP]float64, len(r.rates))
	for k, v := range r.rates {
		out[k] = v
	}
	return out
}
