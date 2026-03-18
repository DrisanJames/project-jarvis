package engine

import (
	"context"
	"database/sql"
	"log"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ISPRateRegistry is a thread-safe registry of per-ISP rate limiters.
// ThrottleAgents call SetRate when they compute new rates.
// The dispatch loop calls AllowN before claiming a batch for each ISP.
//
// When a *sql.DB is attached via SetDB, rate changes are persisted to
// mailing_isp_throttle_state so they survive server restarts.
type ISPRateRegistry struct {
	mu       sync.RWMutex
	limiters map[ISP]*rate.Limiter
	rates    map[ISP]float64 // current msgs/hour for logging
	db       *sql.DB
}

// staleness threshold: persisted rates older than this are ignored on restore
// because ISP conditions likely changed while the server was down.
const throttleStateTTL = 1 * time.Hour

// NewISPRateRegistry creates an empty registry. Call SetRate for each ISP
// to populate it with initial rates before use.
func NewISPRateRegistry() *ISPRateRegistry {
	return &ISPRateRegistry{
		limiters: make(map[ISP]*rate.Limiter),
		rates:    make(map[ISP]float64),
	}
}

// SetDB attaches a database connection for rate persistence.
func (r *ISPRateRegistry) SetDB(db *sql.DB) {
	r.db = db
}

// setRateInternal updates the in-memory limiter and rate map without
// touching the database. Used by RestoreFromDB to avoid writing back
// the same value that was just read.
func (r *ISPRateRegistry) setRateInternal(isp ISP, msgsPerHour float64) {
	perSecond := msgsPerHour / 3600.0
	burst := int(math.Max(1, msgsPerHour/360.0))

	r.mu.Lock()
	if lim, ok := r.limiters[isp]; ok {
		lim.SetLimit(rate.Limit(perSecond))
		lim.SetBurst(burst)
	} else {
		r.limiters[isp] = rate.NewLimiter(rate.Limit(perSecond), burst)
	}
	r.rates[isp] = msgsPerHour
	r.mu.Unlock()
}

// SetRate sets or updates the rate limit for an ISP.
// msgsPerHour is the sustained sending rate (e.g., 500 means 500 messages/hour).
// The burst is set to ~10 seconds of budget (msgsPerHour/360), minimum 1.
// If a DB is attached, the rate is persisted asynchronously.
func (r *ISPRateRegistry) SetRate(isp ISP, msgsPerHour float64) {
	r.setRateInternal(isp, msgsPerHour)

	if r.db != nil {
		go r.persistRate(isp, msgsPerHour)
	}
}

// persistRate writes one ISP's rate to the DB. Runs in a background goroutine
// so SetRate never blocks on I/O.
func (r *ISPRateRegistry) persistRate(isp ISP, msgsPerHour float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (isp) DO UPDATE SET msgs_per_hour = $2, updated_at = NOW()
	`, string(isp), msgsPerHour)
	if err != nil {
		log.Printf("[rate-registry] persist %s rate %.0f/hr failed: %v", isp, msgsPerHour, err)
	}
}

// RestoreFromDB loads persisted throttle rates, applying only rates that are
// lower than the provided config defaults (i.e., the ISP was being throttled).
// Stale entries older than throttleStateTTL are ignored.
func (r *ISPRateRegistry) RestoreFromDB(configRates map[ISP]float64) int {
	if r.db == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := r.db.QueryContext(ctx, `
		SELECT isp, msgs_per_hour, updated_at
		FROM mailing_isp_throttle_state
		WHERE updated_at > $1
	`, time.Now().Add(-throttleStateTTL))
	if err != nil {
		log.Printf("[rate-registry] restore from DB failed: %v", err)
		return 0
	}
	defer rows.Close()

	restored := 0
	for rows.Next() {
		var ispStr string
		var msgsPerHour float64
		var updatedAt time.Time
		if err := rows.Scan(&ispStr, &msgsPerHour, &updatedAt); err != nil {
			continue
		}
		isp := ISP(ispStr)
		configRate, hasConfig := configRates[isp]
		if !hasConfig {
			continue
		}
		if msgsPerHour >= configRate {
			continue
		}
		r.setRateInternal(isp, msgsPerHour)
		restored++
		log.Printf("[rate-registry] restored throttled rate for %s: %.0f/hr (config default: %.0f/hr, saved %s ago)",
			isp, msgsPerHour, configRate, time.Since(updatedAt).Truncate(time.Second))
	}
	return restored
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
